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
// The broker is deliberately thin: after handing over the listener it only
// watches the control channel for EOF (the host going away) and exits,
// removing the listening socket. It performs no protocol parsing, no key
// filtering, and never reads from or writes to client connections.
//
// Usage (see flue on the host side):
//
//	flue -- podman exec \
//	  --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock \
//	  <container> flue-broker
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

var (
	sockPath = flag.String("sock", "/tmp/flue.sock",
		"Unix socket path to listen on inside the container")
	fdArg = flag.String("fd", "3",
		"File descriptor number of the preserved control socket (default 3)")
)

// heldControl keeps the preserved control fd open for the whole process
// lifetime. net.FileConn wraps a file in a Conn and closes the underlying
// fd when the Conn is garbage-collected; if that happened to the preserved
// fd, the number would become invalid and the later WriteMsgUnix in
// sendListener would fail with EBADF. Holding a global *os.File (whose
// finalizer only runs at process exit) keeps the fd alive, and creating a
// new os.File from that number is a cheap dup.
var heldControl *os.File

func main() {
	fdNum, err := strconv.Atoi(*fdArg)
	if err != nil || fdNum < 3 {
		log.Fatalf("invalid -fd %q: must be an integer >= 3", *fdArg)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fdNum, &st); err != nil {
		log.Fatalf("preserved fd %d is not usable: %v", fdNum, err)
	}

	if err := os.Remove(*sockPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("remove stale socket %s: %v", *sockPath, err)
	}

	listener, err := net.Listen("unix", *sockPath)
	if err != nil {
		log.Fatalf("listen %s: %v", *sockPath, err)
	}
	if err := os.Chmod(*sockPath, 0600); err != nil {
		log.Fatalf("chmod %s: %v", *sockPath, err)
	}
	log.Printf("flue-broker: listening on %s, control fd %d", *sockPath, fdNum)
	defer os.Remove(*sockPath)

	// Keep the preserved control fd open for the whole process lifetime so
	// sendListener can build a write conn from it at any point.
	heldControl = os.NewFile(uintptr(fdNum), "flue-control")

	// Hand the listening fd to the host. The host accepts on it from here
	// on; the broker no longer touches the listener.
	if err := sendListener(listener); err != nil {
		log.Fatalf("hand over listener: %v", err)
	}
	log.Printf("flue-broker: handed listener to host, waiting")

	// Open the control socket once for a reader goroutine that detects EOF
	// (host gone). The write side is a separate dup created on demand.
	ctrl, err := net.FileConn(os.NewFile(uintptr(fdNum), "flue-control"))
	if err != nil {
		log.Fatalf("control fd: %v", err)
	}
	defer closeQuiet(ctrl)

	// Block until the control channel closes (host gone) or the process is
	// signalled. The listener keeps serving via the host; this goroutine is
	// only here to notice the host's departure and exit cleanly.
	buf := make([]byte, 64)
	for {
		_, err := ctrl.Read(buf)
		if err != nil {
			if err != io.EOF && err != unix.ECONNRESET {
				log.Printf("flue-broker: control read: %v", err)
			}
			log.Printf("flue-broker: host gone, shutting down")
			return
		}
		// A message arrived on the host->broker direction (should not in the
		// current design); drain it so the buffer does not fill.
	}
}

// sendListener passes the listening socket's fd to the host over the control
// channel using SCM_RIGHTS. It closes only the dup returned by File(); it
// deliberately keeps the *net.UnixListener open because that object owns the
// filesystem path (/tmp/flue.sock) and unlinks it on Close(). Closing it here
// would remove the path that in-container clients connect to, even though the
// host still holds a live fd to the same socket. The listener is left open for
// the broker's lifetime (closed only at process exit, by then redundant).
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
		log.Printf("close listener dup: %v", cerr)
	}
	return nil
}

// closeQuiet closes a connection, logging the error unless it is a benign
// "use of closed network connection".
func closeQuiet(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("close: %v", err)
	}
}
