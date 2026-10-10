package main

import (
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// makeControlPair creates a SOCK_SEQPACKET socketpair and returns both ends
// as net.Conn.
func makeControlPair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	ca, err := net.FileConn(os.NewFile(uintptr(pair[0]), "ctrl-a"))
	if err != nil {
		t.Fatalf("file conn a: %v", err)
	}
	cb, err := net.FileConn(os.NewFile(uintptr(pair[1]), "ctrl-b"))
	if err != nil {
		t.Fatalf("file conn b: %v", err)
	}
	t.Cleanup(func() {
		ca.Close()
		cb.Close()
	})
	return ca, cb
}

// TestReceiveListener verifies that receiveListener reads the single
// listener-fd control message, starts a working accept loop, and that the
// loop stops (acceptDone closes) when the returned listener is closed.
// Unlike the old control-channel EOF approach, this does not depend on the
// netpoller reporting peer closure.
func TestReceiveListener(t *testing.T) {
	host, broker := makeControlPair(t)

	// Simulate the broker: create a real Unix listener and hand its fd to
	// the host over the control channel (SCM_RIGHTS), exactly as flue-broker
	// does, then close the now one-shot control channel.
	l, err := net.Listen("unix", "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	lf, err := l.(*net.UnixListener).File()
	if err != nil {
		t.Fatalf("listener file: %v", err)
	}
	oob := unix.UnixRights(int(lf.Fd()))
	if _, _, err := broker.(*net.UnixConn).WriteMsgUnix([]byte{0}, oob, nil); err != nil {
		lf.Close()
		t.Fatalf("write msg: %v", err)
	}
	lf.Close() // close only the dup; l keeps the path
	broker.Close()

	var wg sync.WaitGroup
	filter := &agentFilter{allow: map[string]struct{}{}}
	listener, acceptDone := receiveListener(host, filter, &wg)

	if listener == nil {
		t.Fatal("receiveListener returned a nil listener")
	}

	// Closing the listener must stop the accept loop (acceptDone closes).
	listener.Close()
	select {
	case <-acceptDone:
		// success: accept loop stopped
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop did not stop after the listener was closed")
	}
	wg.Wait()
}
