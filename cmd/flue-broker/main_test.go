package main

import (
	"errors"
	"net"
	"os"
	"reflect"
	"testing"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// newControlSocketpair creates a SOCK_SEQPACKET socketpair and returns the
// two ends as net.Conn: the "host" end (held by the test) and the "broker"
// end (handed to runChild). Both are closed via t.Cleanup.
func newControlSocketpair(t *testing.T) (host, broker net.Conn) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	h, err := net.FileConn(os.NewFile(uintptr(pair[0]), "test-host"))
	if err != nil {
		t.Fatalf("file conn host: %v", err)
	}
	b, err := net.FileConn(os.NewFile(uintptr(pair[1]), "test-broker"))
	if err != nil {
		t.Fatalf("file conn broker: %v", err)
	}
	t.Cleanup(func() {
		h.Close()
		b.Close()
	})
	return h, b
}

// TestRunChildExitCode verifies that runChild spawns the given command and
// returns its exit status. The control channel is left open (the "host" end
// stays up), so the child-exit path is what ends runChild.
func TestRunChildExitCode(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   int
	}{
		{script: "exit 0", want: 0},
		{script: "exit 7", want: 7},
	} {
		t.Run(tc.script, func(t *testing.T) {
			_, broker := newControlSocketpair(t)
			sockPath := t.TempDir() + "/flue.sock"

			got := runChild([]string{"sh", "-c", tc.script}, sockPath, broker)
			if got != tc.want {
				t.Fatalf("runChild: got exit code %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWithEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		key  string
		val  string
		want []string
	}{
		{"adds missing", []string{"A=1", "B=2"}, "C", "3", []string{"A=1", "B=2", "C=3"}},
		{"replaces existing", []string{"A=1", "C=old", "B=2"}, "C", "3", []string{"A=1", "B=2", "C=3"}},
		{"keeps longer key", []string{"A=1", "CX=1", "B=2"}, "C", "3", []string{"A=1", "CX=1", "B=2", "C=3"}},
		{"empty env", nil, "SSH_AUTH_SOCK", "/tmp/x.sock", []string{"SSH_AUTH_SOCK=/tmp/x.sock"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withEnv(tc.env, tc.key, tc.val)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("withEnv = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChildExitCode(t *testing.T) {
	if got := childExitCode(nil); got != 0 {
		t.Fatalf("nil err: got %d, want 0", got)
	}
	if got := childExitCode(errors.New("boom")); got != 1 {
		t.Fatalf("non-exit err: got %d, want 1", got)
	}
}
