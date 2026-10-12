package main

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestReceiveListenerBrokerCredentials verifies that when the host end has
// SO_PASSCRED enabled (as runChildWithAgent does), the handover message
// carries the sender's SCM_CREDENTIALS and receiveListener records the
// sender's session id on the policy. Here the "broker" is the test process
// itself, so the expected value is its own session id.
func TestReceiveListenerBrokerCredentials(t *testing.T) {
	host, broker := makeControlPair(t)

	// Enable SO_PASSCRED on the host end, exactly as runChildWithAgent
	// does.
	rc, err := host.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	var hfd int
	_ = rc.Control(func(fd uintptr) { hfd = int(fd) })
	if err := unix.SetsockoptInt(hfd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		t.Fatalf("setsockopt SO_PASSCRED: %v", err)
	}

	// Hand over a real Unix listener, exactly as the broker does.
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

	var pol policy
	var cs clientSet
	listener, acceptDone := receiveListener(host, &pol, &cs)
	if listener == nil {
		t.Fatal("receiveListener returned a nil listener")
	}
	listener.Close()
	<-acceptDone

	want, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	if pol.brokerSession != want {
		t.Fatalf("brokerSession = %d, want %d (the writer's own session)", pol.brokerSession, want)
	}
}

// TestSessionIDFromStatLine verifies the /proc/<pid>/stat parser: the
// session id (field 6) must be found even when the comm field (2) contains
// spaces or parentheses — the parse starts after the last `)`.
func TestSessionIDFromStatLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		want    int
		wantErr bool
	}{
		{"comm with spaces", "4242 (my comm) S 1 2 4242 0 -1 4194305 0 0 0 0 0 0 20 0 1 0 100 18446744073709551615 123 0 0", 4242, false},
		{"comm with parens", "7 (a (b (c)) d) S 1 7 7 7 0", 7, false},
		{"plain", "9 (comm) S 1 9 9 0", 9, false},
		{"no closing paren", "4242 (unclosed S 1 2 3", 0, true},
		{"too few fields", "42 (x) S 1", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sessionIDFromStatLine(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("session = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sessionIDFromStatLine: %v", err)
			}
			if got != tc.want {
				t.Fatalf("session = %d, want %d", got, tc.want)
			}
		})
	}

	// And against the real /proc/self/stat: the session must be positive
	// and stable across two reads.
	a, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	b, err := sessionIDOfPID(os.Getpid())
	if err != nil {
		t.Fatalf("sessionIDOfPID(self): %v", err)
	}
	if a <= 0 || a != b {
		t.Fatalf("own session %d then %d, want the same positive value", a, b)
	}
}
