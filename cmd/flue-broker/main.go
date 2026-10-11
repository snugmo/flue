// Command flue-broker is the container-side half of the flue SSH agent
// bridge.
//
// It listens on a Unix socket inside the container (a unique per-session
// path by default; -sock pins one) so that ssh / ssh-add / git can use it
// (via SSH_AUTH_SOCK). Rather than
// proxying each client connection itself, it hands the *listening* fd to
// the host (flue) over a control socketpair (fd 3, received via
// podman exec --preserve-fds) using SCM_RIGHTS. The host then accepts
// connections directly on that listener and proxies each one to the real
// agent with filtering applied.
//
// The broker is deliberately thin: after handing over the listener it
// performs no protocol parsing, no key filtering, and never reads from or
// writes to client connections.
//
// Two modes:
//
//  1. Standalone (no arguments): the broker hands over the listener and
//     blocks until the control channel reports the host gone (EOF), then
//     exits and removes the socket. This is the original behavior.
//
//  2. Wrapper (one or more arguments): the broker hands over the listener,
//     then launches the given command as a child process inside the
//     container (e.g. `flue-broker bash`) with SSH_AUTH_SOCK pointed at its
//     socket, and waits for it. The child's stdio is wired straight through,
//     so an interactive shell works. The broker exits with the child's exit
//     status. If the host's departure is detected first (via the control
//     channel, best-effort), the child is killed and the broker exits 1.
//
// Usage (see flue on the host side). Wrapper mode — get a shell inside the
// container with the agent available (the broker sets SSH_AUTH_SOCK for
// the child, so -e is not required):
//
//	flue exec -- podman exec --preserve-fds 1 <container> flue-broker bash
//
// …or just `flue enter <container> [command…]`, which runs that wrapper-
// mode podman exec for you (defaulting the command to /bin/sh) and
// injects the broker in memory by default. Standalone mode (no command)
// leaves SSH_AUTH_SOCK to the caller, so pin -sock to a fixed path:
//
//	flue exec -- podman exec --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> \
//	  flue-broker -sock /tmp/flue.sock
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	sockPath = flag.String("sock", "/tmp/flue.sock",
		"Unix socket path to listen on inside the container")
	fdArg = flag.String("fd", "3",
		"File descriptor number of the preserved control socket (default 3)")
	logLevel = flag.String("log-level", "info",
		"log level: debug, info, warn, or error")
)

// log is the package-level slog logger. The default is rebuilt in main
// from the -log-level flag; the declaration-time default keeps tests
// (which never call main) working.
var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// parseLevel maps a -log-level flag value to a slog.Level.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", s)
}

// fatalf prints the formatted message to stderr and exits 1, keeping the
// traditional CLI fatal behavior (a plain-text line, as log.Fatalf used
// to produce) rather than a slog record — usage and flag errors in
// particular must read as ordinary text. The format string is
// printf-substituted; note that slog does not interpret format verbs (it
// treats the args as key/value pairs). slog is used for the non-fatal,
// leveled operational logging.
func fatalf(format string, args ...any) {
	os.Exit(failf(format, args...))
}

// failf prints the formatted message to stderr like fatalf but returns the
// exit code (1) instead of exiting, so callers with deferred cleanup can
// return it.
func failf(format string, args ...any) int {
	fmt.Fprintln(os.Stderr, fmt.Sprintf(format, args...))
	return 1
}

// checkControlFD validates that fdNum is the control socketpair end that
// flue hands to the command. A plain fstat cannot catch the common
// mistake of running the exec without `--preserve-fds 1`: the fd number
// is open in that case too (the runtime or the exec allocated it to
// something else, e.g. a pipe), so the failure would surface only later
// and deep inside sendListener as an opaque "socket operation on
// non-socket". Getting the socket type up front turns that into an
// actionable message.
func checkControlFD(fdNum int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fdNum, &st); err != nil {
		return fmt.Errorf("preserved fd %d is not open: %v (run the exec with --preserve-fds 1 and make sure -fd matches the fd that flue passes, default 3)", fdNum, err)
	}
	typ, err := unix.GetsockoptInt(fdNum, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil {
		return fmt.Errorf("preserved fd %d is open but is not a socket: %v (want the SOCK_SEQPACKET control socketpair from flue — run the exec with --preserve-fds 1 and make sure -fd matches the fd that flue passes, default 3)", fdNum, err)
	}
	if typ != unix.SOCK_SEQPACKET {
		return fmt.Errorf("preserved fd %d is a SOCK_%s socket, but flue passes a SOCK_SEQPACKET control socketpair: check the --preserve-fds count and the -fd value", fdNum, sockTypeName(typ))
	}
	return nil
}

// sockTypeName gives a short name for the socket types that a preserved
// fd is realistically mistaken for; anything else prints the raw number.
func sockTypeName(typ int) string {
	switch typ {
	case unix.SOCK_STREAM:
		return "STREAM"
	case unix.SOCK_DGRAM:
		return "DGRAM"
	case unix.SOCK_SEQPACKET:
		return "SEQPACKET"
	case unix.SOCK_RAW:
		return "RAW"
	case unix.SOCK_PACKET:
		return "PACKET"
	}
	return fmt.Sprintf("%d", typ)
}

func main() {
	// run returns the exit code rather than calling os.Exit itself, so the
	// defers inside it (remove the socket, close the control conn) have
	// run by the time the process exits.
	os.Exit(run())
}

// run is the broker proper; it returns the process exit code.
func run() int {
	flag.Parse()

	if lvl, err := parseLevel(*logLevel); err != nil {
		return failf("flue-broker: %v", err)
	} else {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	}

	fdNum, err := strconv.Atoi(*fdArg)
	if err != nil || fdNum < 3 {
		return failf("flue-broker: invalid -fd %q: must be an integer >= 3", *fdArg)
	}

	if err := checkControlFD(fdNum); err != nil {
		return failf("flue-broker: %v", err)
	}

	// When -sock is not given explicitly, generate a per-session socket path
	// (random name in the best available runtime directory) so concurrent
	// `flue enter` sessions don't collide on a single fixed path.
	if !sockFlagSet() {
		*sockPath = generateSocketPath()
		log.Debug("flue-broker: generated socket path", "sock", *sockPath)
	}
	if err := os.Remove(*sockPath); err != nil && !os.IsNotExist(err) {
		return failf("flue-broker: remove stale socket %s: %v", *sockPath, err)
	}

	listener, err := listenPrivate(*sockPath)
	if err != nil {
		return failf("flue-broker: listen %s: %v", *sockPath, err)
	}
	// Registered before any later step can fail, so every return path
	// removes the socket file.
	defer removeQuiet(*sockPath)
	log.Info("flue-broker: listening", "sock", *sockPath, "control-fd", fdNum)

	// Wrap the preserved fd in a conn exactly once. net.FileConn dups the
	// fd, so the os.File is closed right away: it would otherwise stay
	// open until its finalizer ran, and that finalizer would close the
	// original fd number out from under anything else holding it. The one
	// conn serves both the listener handover and the host-gone watcher.
	f := os.NewFile(uintptr(fdNum), "flue-control")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return failf("flue-broker: control fd: %v", err)
	}
	ctrl := c.(*net.UnixConn)
	defer closeQuiet(ctrl)

	// Hand the listening fd to the host. The host accepts on it from here
	// on; the broker no longer touches the listener. The listener stays open
	// for the broker's lifetime so the socket path it owns remains in place
	// for in-container clients to connect to.
	if err := sendListener(ctrl, listener); err != nil {
		return failf("flue-broker: hand over listener: %v", err)
	}
	log.Debug("flue-broker: handed listener to host")

	// Branch on mode: with arguments, run the wrapped child and return its
	// exit status; without, block until the host goes away. The code is
	// returned to main so the defers above (remove socket, close control)
	// run before the process exits with it.
	if args := flag.Args(); len(args) > 0 {
		return runChild(args, *sockPath, ctrl)
	}
	return waitForHost(ctrl)
}

// listenPrivate listens on a Unix socket at path with mode 0600 from the
// moment it exists. A chmod after Listen would leave a window in which the
// socket carries the umask-derived mode (typically world-connectable);
// tightening the umask around the bind closes it. The broker is still
// single-threaded here, so the process-wide umask change is not observable
// by anything else.
func listenPrivate(path string) (net.Listener, error) {
	old := unix.Umask(0o177)
	defer unix.Umask(old)
	return net.Listen("unix", path)
}

// waitForHost blocks until the control channel reports the host going away
// (EOF / reset) and returns 0 for a clean shutdown. This is the standalone
// (no-argument) mode.
func waitForHost(ctrl net.Conn) int {
	sigc := notifySignals()
	defer signal.Stop(sigc)
	select {
	case <-watchHostGone(ctrl, nil):
		log.Info("flue-broker: host gone, shutting down")
	case sig := <-sigc:
		// Return (rather than die) so run's defers remove the socket.
		log.Info("flue-broker: signalled, shutting down", "signal", sig)
	}
	return 0
}

// notifySignals subscribes to the termination signals the broker handles
// itself, so that it unwinds (removing its socket) instead of dying with
// the file left behind.
func notifySignals() chan os.Signal {
	c := make(chan os.Signal, 4)
	signal.Notify(c, unix.SIGTERM, unix.SIGHUP, unix.SIGINT)
	return c
}

// watchHostGone starts a goroutine that reads the control channel and
// closes the returned channel when the peer is gone (EOF / reset), i.e.
// the host has departed. The channel is closed by the goroutine's deferred
// cleanup, so receiving on it also serves as a wait for the goroutine to
// finish. If *shuttingDown is set before a read fails, the failure is our
// own orderly close and the watcher exits quietly; any other unexpected
// read error is logged. The host-gone event itself is not logged here:
// the two callers treat it differently (standalone: clean shutdown;
// wrapper: the child must be killed).
func watchHostGone(ctrl net.Conn, shuttingDown *atomic.Bool) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		buf := make([]byte, 64)
		for {
			_, err := ctrl.Read(buf)
			if err == nil {
				// A message arrived on the host->broker direction (should
				// not in the current design); drain it so the buffer does
				// not fill.
				continue
			}
			if shuttingDown != nil && shuttingDown.Load() {
				// We closed the channel during an orderly shutdown.
				return
			}
			if err != io.EOF && err != unix.ECONNRESET {
				log.Warn("flue-broker: control read", "err", err)
			}
			return
		}
	}()
	return gone
}

// runChild starts args[0] (plus args[1:]) as a child process inside the
// container, wires its stdio through, and points SSH_AUTH_SOCK at the
// broker's socket so ssh / git in the child reach the host agent through
// flue. It blocks until the child exits (returning the child's exit status)
// or the host goes away first (killing the child and returning 1).
func runChild(args []string, sockPath string, ctrl net.Conn) int {
	cmdPath := args[0]
	if !strings.Contains(cmdPath, "/") {
		if resolved, err := exec.LookPath(cmdPath); err == nil {
			cmdPath = resolved
		}
	}

	cmd := exec.Command(cmdPath, args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withEnv(os.Environ(), "SSH_AUTH_SOCK", sockPath)

	log.Info("flue-broker: running", "cmd", args, "sock", sockPath)

	// Keep the broker's socket fds (the control socketpair and the listener)
	// out of the child's fd table. CLOEXEC only affects the exec'd child;
	// the broker keeps using those fds. This stops the child — an arbitrary
	// program such as bash — from inheriting, and writing to, the control
	// channel or the listener.
	markCloexecAll()

	if err := cmd.Start(); err != nil {
		return failf("flue-broker: start %s: %v", cmdPath, err)
	}

	// Watch the control channel for the host going away. The watcher closes
	// hostGone when the peer is gone. shuttingDown is set when we close the
	// control channel ourselves (child exited) so the watcher exits quietly
	// instead of treating it as a host departure.
	var shuttingDown atomic.Bool
	hostGone := watchHostGone(ctrl, &shuttingDown)

	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()

	// SIGTERM and SIGHUP aimed at the broker are forwarded to the child,
	// which decides how to react; the broker keeps running until the child
	// exits and then cleans up as usual. SIGINT is swallowed: on a tty the
	// child already receives its own copy via the foreground process group,
	// and the broker must outlive it to remove the socket.
	sigc := notifySignals()
	defer signal.Stop(sigc)
	stopFwd := make(chan struct{})
	defer close(stopFwd)
	go func() {
		for {
			select {
			case sig := <-sigc:
				if sig != unix.SIGINT {
					_ = cmd.Process.Signal(sig)
				}
			case <-stopFwd:
				return
			}
		}
	}()

	select {
	case <-hostGone:
		// The host went away, so the agent socket is no longer served. Kill
		// the child and exit non-zero.
		log.Warn("flue-broker: host gone")
		_ = cmd.Process.Kill()
		<-childDone
		log.Warn("flue-broker: child killed after host departure")
		return 1
	case err := <-childDone:
		code := childExitCode(err)
		shuttingDown.Store(true)
		_ = ctrl.Close() // unblock the control-channel watcher
		<-hostGone
		log.Debug("flue-broker: child exited", "code", code)
		return code
	}
}

// childExitCode maps a Wait error to a process exit code (0 on success).
func childExitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return exitStatus(ee)
	}
	return 1
}

// exitStatus maps an ExitError to a shell-style exit code: the exit status,
// or 128+signal when the process was killed by a signal (ExitCode alone
// reports -1 for that, which os.Exit would turn into 255).
func exitStatus(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// withEnv returns a copy of env with key set to val, replacing any existing
// entry for key and keeping the relative order of the rest.
func withEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+val)
}

// sockFlagSet reports whether the -sock flag was given on the command line.
func sockFlagSet() bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "sock" {
			set = true
		}
	})
	return set
}

// generateSocketPath returns a per-session Unix socket path: a random name
// under the best available runtime directory.
func generateSocketPath() string {
	return filepath.Join(pickSocketDir(), "flue-"+randHex(8)+".sock")
}

// pickSocketDir chooses the directory for the generated socket: XDG_RUNTIME_DIR
// (if set and present), else /run/user/<uid>, else /run, else /tmp — first
// that is writable wins.
func pickSocketDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && isDir(d) {
		return d
	}
	if d := filepath.Join("/run/user", strconv.Itoa(os.Getuid())); writableDir(d) {
		return d
	}
	if writableDir("/run") {
		return "/run"
	}
	return "/tmp"
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// writableDir reports whether a file can be created in dir.
func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".flue-wtest-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// randHex returns up to n hex chars of cryptographic randomness (falling
// back to the pid if the entropy source fails).
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", os.Getpid())
	}
	return fmt.Sprintf("%x", b)
}

// markCloexecAll sets FD_CLOEXEC on every open descriptor above 2, so that a
// child started afterwards does not inherit the broker's socket fds. The
// broker itself keeps using those fds — CLOEXEC only affects the exec'd
// child. It enumerates open descriptors via /proc/self/fd (the broker is
// Linux-only).
func markCloexecAll() {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 {
			continue
		}
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	}
}

// sendListener passes the listening socket's fd to the host over the control
// channel using SCM_RIGHTS. It closes only the dup returned by File(); it
// deliberately keeps the *net.UnixListener open because that object owns the
// filesystem path (/tmp/flue.sock) and in-container clients connect to it by
// name. Closing it here would remove the path even though the host still
// holds a live fd to the same socket. The listener is left open for the
// broker's lifetime (closed only at process exit, by then redundant).
func sendListener(ctrl *net.UnixConn, listener net.Listener) error {
	ln, ok := listener.(*net.UnixListener)
	if !ok {
		return errors.New("listener is not a Unix socket")
	}
	f, err := ln.File()
	if err != nil {
		return err
	}
	// Close only the dup, on every path; keep ln (and thus the path) open.
	defer func() {
		if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			log.Debug("flue-broker: close listener dup", "err", cerr)
		}
	}()

	oob := unix.UnixRights(int(f.Fd()))
	_, _, err = ctrl.WriteMsgUnix([]byte{0}, oob, nil)
	return err
}

// removeQuiet removes a file, logging the error unless it is a benign
// "file does not exist" (the socket was already gone). Used for
// best-effort cleanup of the broker's own socket path.
func removeQuiet(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Debug("flue-broker: remove", "path", path, "err", err)
	}
}

// closeQuiet closes a connection, logging the error unless it is a benign
// "use of closed network connection".
func closeQuiet(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Debug("flue-broker: close", "err", err)
	}
}
