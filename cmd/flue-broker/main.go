// Command flue-broker is the container-side half of the flue SSH agent
// bridge.
//
// It listens on a Unix socket inside the container (default /tmp/flue.sock)
// so that ssh / ssh-add / git can use it (via SSH_AUTH_SOCK). Rather than
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
// Usage (see flue on the host side):
//
//	flue -- podman exec \
//	  --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock \
//	  <container> flue-broker
//
// Wrapper mode — get a shell inside the container with the agent available
// (the broker sets SSH_AUTH_SOCK for the child, so -e is not required):
//
//	flue -- podman exec \
//	  --preserve-fds 1 \
//	  <container> flue-broker bash
//
// …or just `flue enter <container> [command…]`, which runs that wrapper-
// mode podman exec for you (defaulting the command to /bin/sh).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"

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

// fatalf logs at error level and exits 1, mirroring log.Fatalf. The
// format string is printf-substituted before logging: slog's Error does
// not interpret format verbs (it treats the args as key/value pairs).
func fatalf(format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// heldControl keeps the preserved control fd open for the whole process
// lifetime. net.FileConn wraps a file in a Conn and closes the underlying
// fd when the Conn is garbage-collected; if that happened to the preserved
// fd, the number would become invalid and the later WriteMsgUnix in
// sendListener would fail with EBADF. Holding a global *os.File (whose
// finalizer only runs at process exit) keeps the fd alive, and creating a
// new os.File from that number is a cheap dup.
var heldControl *os.File

func main() {
	flag.Parse()

	if lvl, err := parseLevel(*logLevel); err != nil {
		fatalf("flue-broker: %v", err)
	} else {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	}

	fdNum, err := strconv.Atoi(*fdArg)
	if err != nil || fdNum < 3 {
		fatalf("flue-broker: invalid -fd %q: must be an integer >= 3", *fdArg)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fdNum, &st); err != nil {
		fatalf("flue-broker: preserved fd %d is not usable: %v", fdNum, err)
	}

	if err := os.Remove(*sockPath); err != nil && !os.IsNotExist(err) {
		fatalf("flue-broker: remove stale socket %s: %v", *sockPath, err)
	}

	listener, err := net.Listen("unix", *sockPath)
	if err != nil {
		fatalf("flue-broker: listen %s: %v", *sockPath, err)
	}
	if err := os.Chmod(*sockPath, 0600); err != nil {
		fatalf("flue-broker: chmod %s: %v", *sockPath, err)
	}
	log.Info("flue-broker: listening", "sock", *sockPath, "control-fd", fdNum)
	defer removeQuiet(*sockPath)

	// Keep the preserved control fd open for the whole process lifetime so
	// sendListener can build a write conn from it at any point.
	heldControl = os.NewFile(uintptr(fdNum), "flue-control")

	// Hand the listening fd to the host. The host accepts on it from here
	// on; the broker no longer touches the listener. The listener stays open
	// for the broker's lifetime so the socket path it owns remains in place
	// for in-container clients to connect to.
	if err := sendListener(listener); err != nil {
		fatalf("flue-broker: hand over listener: %v", err)
	}
	log.Debug("flue-broker: handed listener to host")

	// Open the control socket once for a reader that detects EOF (host
	// gone). The write side is a separate dup created on demand.
	ctrl, err := net.FileConn(os.NewFile(uintptr(fdNum), "flue-control"))
	if err != nil {
		fatalf("flue-broker: control fd: %v", err)
	}
	defer closeQuiet(ctrl)

	// Branch on mode: with arguments, run the wrapped child and return its
	// exit status; without, block until the host goes away. The code is
	// returned to main so the defers above (remove socket, close control)
	// run before we os.Exit with it.
	var code int
	if args := flag.Args(); len(args) > 0 {
		code = runChild(args, *sockPath, ctrl)
	} else {
		code = waitForHost(ctrl)
	}

	if code != 0 {
		os.Exit(code)
	}
}

// waitForHost blocks until the control channel reports the host going away
// (EOF / reset) and returns 0 for a clean shutdown. This is the standalone
// (no-argument) mode.
func waitForHost(ctrl net.Conn) int {
	buf := make([]byte, 64)
	for {
		_, err := ctrl.Read(buf)
		if err != nil {
			if err != io.EOF && err != unix.ECONNRESET {
				log.Warn("flue-broker: control read", "err", err)
			}
			log.Info("flue-broker: host gone, shutting down")
			return 0
		}
		// A message arrived on the host->broker direction (should not in the
		// current design); drain it so the buffer does not fill.
	}
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
		fatalf("flue-broker: start %s: %v", cmdPath, err)
	}

	// Watch the control channel for the host going away. A reader goroutine
	// closes hostGone on EOF/reset. shuttingDown is set when we close the
	// control channel ourselves (child exited) so the reader stops quietly
	// instead of logging a spurious "host gone".
	hostGone := make(chan struct{})
	readerDone := make(chan struct{})
	var shuttingDown atomic.Bool
	go func() {
		defer close(readerDone)
		buf := make([]byte, 64)
		for {
			_, err := ctrl.Read(buf)
			if err != nil {
				if shuttingDown.Load() {
					// We closed the channel during an orderly shutdown.
					return
				}
				if err != io.EOF && err != unix.ECONNRESET {
					log.Warn("flue-broker: control read", "err", err)
				}
				log.Warn("flue-broker: host gone")
				close(hostGone)
				return
			}
		}
	}()

	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()

	select {
	case <-hostGone:
		// The host went away, so the agent socket is no longer served. Kill
		// the child and exit non-zero.
		_ = cmd.Process.Kill()
		<-childDone
		<-readerDone // reader already exited after closing hostGone
		log.Warn("flue-broker: child killed after host departure")
		return 1
	case err := <-childDone:
		code := childExitCode(err)
		shuttingDown.Store(true)
		_ = ctrl.Close() // unblock the control reader
		<-readerDone
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
		return ee.ExitCode()
	}
	return 1
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
func sendListener(listener net.Listener) error {
	ln, ok := listener.(*net.UnixListener)
	if !ok {
		return errors.New("listener is not a Unix socket")
	}
	f, err := ln.File()
	if err != nil {
		return err
	}
	listenFD := int(f.Fd())

	ctrl, err := net.FileConn(os.NewFile(heldControl.Fd(), "flue-control-write"))
	if err != nil {
		return err
	}
	defer closeQuiet(ctrl)

	oob := unix.UnixRights(listenFD)
	if _, _, err := ctrl.(*net.UnixConn).WriteMsgUnix([]byte{0}, oob, nil); err != nil {
		return err
	}

	// Close only the dup; keep ln (and thus the path) open.
	if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
		log.Debug("flue-broker: close listener dup", "err", cerr)
	}
	return nil
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
