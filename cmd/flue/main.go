// Command flue is the container-side half of the flue SSH agent bridge.
//
// It listens on a Unix socket inside the container (default /tmp/flue.sock)
// so that ssh / ssh-add / git can use it (via SSH_AUTH_SOCK). For each
// client connection, it sends the connected fd to the host over a control
// socketpair (fd 3, received via podman exec --preserve-fds) using
// SCM_RIGHTS, then closes the local copy. No protocol parsing or key
// filtering happens here: the host side (fluehost) proxies each
// connection to the real agent with filtering applied.
//
// flue exits when the control socket returns EOF (the host has gone away),
// removing the listening socket.
//
// Usage (see fluehost on the host side):
//
//	fluehost -- podman exec \
//	  --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock \
//	  <container> flue
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	sockPath = flag.String("sock", "/tmp/flue.sock",
		"Unix socket path to listen on inside the container")
	fdArg = flag.String("fd", "3",
		"File descriptor number of the preserved control socket (default 3)")
	verbose = flag.Bool("verbose", false,
		"log each forwarded client connection to stderr")
)

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
	log.Printf("flue: listening on %s, control fd %d", *sockPath, fdNum)

	// Open the control socket once for a reader goroutine that detects
	// EOF (host gone). Each forwarder opens its own copy for writing.
	ctrl, err := net.FileConn(os.NewFile(uintptr(fdNum), "flue-control"))
	if err != nil {
		log.Fatalf("control fd: %v", err)
	}
	defer closeQuiet(ctrl)

	done := make(chan struct{})
	go watchControl(ctrl, done)

	var wg sync.WaitGroup

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-done:
				log.Printf("flue: host gone, shutting down")
				wg.Wait()
				return
			default:
			}
			// Transient accept error; retry unless the host is gone.
			continue
		}
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			if err := sendToHost(conn, fdNum); err != nil {
				log.Printf("forward client: %v", err)
			} else if *verbose {
				log.Printf("forwarded client")
			}
		}(conn)
	}
}

// watchControl blocks on the control socket until the host closes it
// (EOF or error), then closes done.
func watchControl(ctrl net.Conn, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 64)
	for {
		_, err := ctrl.Read(buf)
		if err != nil {
			if err != io.EOF && err != unix.ECONNRESET {
				log.Printf("flue: control read: %v", err)
			}
			return
		}
		// A message arrived (shouldn't happen on this direction, but
		// drain it so the buffer doesn't fill).
	}
}

// closeQuiet closes a connection, logging the error unless it is a
// benign "use of closed network connection".
func closeQuiet(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("close: %v", err)
	}
}

// sendToHost hands the client connection's fd to the host over the control
// socket, then closes the local copy. The host proxies the connection to
// the real agent.
func sendToHost(conn net.Conn, fdNum int) error {
	defer closeQuiet(conn)

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("client is not a Unix socket")
	}
	f, err := uc.File()
	if err != nil {
		return fmt.Errorf("get fd: %w", err)
	}
	if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
		log.Printf("close client fd: %v", cerr)
	}

	ctrl, err := net.FileConn(os.NewFile(uintptr(fdNum), "flue-control"))
	if err != nil {
		return fmt.Errorf("control fd: %w", err)
	}
	defer closeQuiet(ctrl)

	// One byte of payload; the fd travels in a SCM_RIGHTS control message.
	oob := unix.UnixRights(int(f.Fd()))
	if _, _, err := ctrl.(*net.UnixConn).WriteMsgUnix(
		[]byte{0}, oob, nil); err != nil {
		return fmt.Errorf("send fd: %w", err)
	}
	return nil
}
