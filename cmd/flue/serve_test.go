package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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

	var wg clientSet
	pol := &policy{}
	listener, acceptDone, rerr := receiveListener(host, pol, &wg)
	if rerr != nil {
		t.Fatalf("receiveListener: %v", rerr)
	}

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

// socketInodeOf returns the kernel socket inode behind fd (from the
// /proc/self/fd/<fd> symlink "socket:[inode]"), or "" if fd is not a socket.
func socketInodeOf(t *testing.T, fd int) string {
	t.Helper()
	link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		t.Fatalf("readlink /proc/self/fd/%d: %v", fd, err)
	}
	i := strings.Index(link, "socket:[")
	if i < 0 {
		return ""
	}
	rest := link[i+len("socket:["):]
	if j := strings.IndexByte(rest, ']'); j > 0 {
		return rest[:j]
	}
	return ""
}

// openSocketsContains reports whether any fd in this process still
// references the socket with the given inode.
func openSocketsContains(inode string) bool {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false
	}
	for _, e := range ents {
		link, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err == nil && strings.Contains(link, "socket:["+inode+"]") {
			return true
		}
	}
	return false
}

// TestReceiveListenerRejectsWrongFds verifies that receiveListener rejects
// an fd that is not a listening Unix stream socket: a SOCK_SEQPACKET
// socket (right domain, wrong type), an un-listened stream socket
// (SO_ACCEPTCONN 0), and a TCP listener (wrong domain). Each rejection
// must return no listener and close the received copy of the fd (the
// last condition checked via the socket inode: the test closes its own
// reference first, so a live socket in the fd table is flue's copy).
func TestReceiveListenerRejectsWrongFds(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(t *testing.T) (fd int, inode string, closeRef func())
	}{
		{"seqpacket socket (wrong type)", func(t *testing.T) (int, string, func()) {
			t.Helper()
			fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
			if err != nil {
				t.Fatalf("socket: %v", err)
			}
			return fd, socketInodeOf(t, fd), func() { _ = unix.Close(fd) }
		}},
		{"unlistened stream socket (SO_ACCEPTCONN 0)", func(t *testing.T) (int, string, func()) {
			t.Helper()
			fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("socket: %v", err)
			}
			return fd, socketInodeOf(t, fd), func() { _ = unix.Close(fd) }
		}},
		{"tcp listener (wrong domain)", func(t *testing.T) (int, string, func()) {
			t.Helper()
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("cannot listen on TCP in this environment: %v", err)
			}
			rc, err := l.(*net.TCPListener).SyscallConn()
			if err != nil {
				l.Close()
				t.Fatalf("syscall conn: %v", err)
			}
			var fd int
			_ = rc.Control(func(rawFD uintptr) { fd = int(rawFD) })
			return fd, socketInodeOf(t, fd), func() { _ = l.Close() }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, inode, closeRef := tc.mk(t)
			t.Cleanup(closeRef)

			host, broker := makeControlPair(t)
			oob := unix.UnixRights(fd)
			if _, _, err := broker.(*net.UnixConn).WriteMsgUnix([]byte{0}, oob, nil); err != nil {
				t.Fatalf("write msg: %v", err)
			}
			closeRef() // drop the test's reference before the leak check

			var cs clientSet
			listener, acceptDone, rerr := receiveListener(host, &policy{}, &cs)
			if rerr != nil {
				t.Fatalf("receiveListener: %v", rerr)
			}
			if listener != nil {
				t.Fatalf("receiveListener accepted a %s fd", tc.name)
			}
			<-acceptDone // a rejected handover returns a closed channel
			if inode != "" && openSocketsContains(inode) {
				t.Error("received fd copy is still open after rejection")
			}
		})
	}
}

// TestClientSetCloseAll verifies that closeAll unblocks a handler stuck on
// a lingering client and that later connections are refused.
func TestClientSetCloseAll(t *testing.T) {
	var cs clientSet
	srv, cli := net.Pipe()
	defer cli.Close()
	if !cs.add(srv) {
		t.Fatal("add before closeAll refused")
	}
	finished := make(chan struct{})
	go func() {
		defer cs.done(srv)
		_, _ = srv.Read(make([]byte, 1)) // blocks until srv is closed
		close(finished)
	}()

	cs.closeAll()
	cs.Wait()
	<-finished

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if cs.add(a) {
		t.Fatal("add after closeAll accepted")
	}
}

// TestHandleClientShutdownUnblocksStalledAgent is the regression test for
// a hung shutdown: the host agent accepts the upstream connection and
// never answers (for example, it is blocked on an askpass confirmation
// the user lost, and the user exited with it pending). ServeAgent blocks
// in the upstream read, which closing the client side (closeAll) cannot
// unblock — closeAll must close the upstream too, and the handler must
// return.
func TestHandleClientShutdownUnblocksStalledAgent(t *testing.T) {
	// A fake host agent: it accepts and, once the request is off the
	// wire, goes silent.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agent.sock")
	agentL, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	requestSeen := make(chan struct{})
	hold := make(chan struct{})
	t.Cleanup(func() {
		close(hold) // release the per-connection goroutines (goleak)
		agentL.Close() // end the accept loop
	})
	go func() {
		for {
			c, err := agentL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// The forwarded request: a 4-byte big-endian length
				// (1) and the request byte.
				if _, err := io.ReadFull(c, make([]byte, 5)); err == nil {
					close(requestSeen) // the request is off the wire
				}
				<-hold
			}(c)
		}
	}()

	clientEnd, testEnd := net.Pipe()
	t.Cleanup(func() {
		clientEnd.Close()
		testEnd.Close()
	})

	pol := &policy{sock: sockPath}
	var cs clientSet
	if !cs.add(clientEnd) {
		t.Fatal("add refused")
	}
	finished := make(chan struct{})
	go func() {
		defer cs.done(clientEnd)
		handleClient(clientEnd, pol, &cs)
		close(finished)
	}()

	// A list request in the current agent protocol: a 4-byte big-endian
	// length, then opcode 11 (requestIdentities). The handler dials the
	// fake agent, forwards the request, and blocks waiting for a reply
	// that never comes.
	if _, err := testEnd.Write([]byte{0, 0, 0, 1, 11}); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-requestSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the fake agent")
	}

	cs.closeAll()
	cs.Wait()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after closeAll (stalled upstream not closed)")
	}
}

// TestReceiveListenerTimeout verifies that a broker that never sends the
// listener does not block receiveListener forever.
func TestReceiveListenerTimeout(t *testing.T) {
	old := handoverTimeout
	handoverTimeout = 50 * time.Millisecond
	defer func() { handoverTimeout = old }()

	host, _ := makeControlPair(t) // the peer stays open and silent
	var cs clientSet
	done := make(chan net.Listener, 1)
	go func() {
		l, _, rerr := receiveListener(host, &policy{}, &cs)
		if rerr != nil {
			t.Errorf("receiveListener: %v", rerr)
		}
		done <- l
	}()
	select {
	case l := <-done:
		if l != nil {
			t.Fatal("got a listener from a silent peer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiveListener did not time out")
	}
}

// TestClientSetCap verifies the live-connection cap, and that a finished
// connection frees its slot.
func TestClientSetCap(t *testing.T) {
	cs := clientSet{max: 1}
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	if !cs.add(a1) {
		t.Fatal("first add refused")
	}
	if cs.add(b1) {
		t.Fatal("add over the cap accepted")
	}
	cs.done(a1)
	if !cs.add(b1) {
		t.Fatal("add after a slot freed refused")
	}
	cs.done(b1)
}

// TestIdleConn verifies that a silent peer makes Read fail once the idle
// timeout passes.
func TestIdleConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &idleConn{Conn: a, timeout: 30 * time.Millisecond}
	if _, err := c.Read(make([]byte, 1)); !os.IsTimeout(err) {
		t.Fatalf("Read on idle conn: got %v, want a timeout", err)
	}
}
