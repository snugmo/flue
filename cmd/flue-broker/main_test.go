package main

import (
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestMarkCloexecAll verifies the invariant that runChild relies on: after
// markCloexecAll, no fd above 2 survives into a wrapped child, so the control
// socketpair and listener are never leaked to processes like bash.
func TestMarkCloexecAll(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	a, b := pair[0], pair[1]
	defer unix.Close(a)
	defer unix.Close(b)

	markCloexecAll()

	for _, fd := range []int{a, b} {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("F_GETFD(%d): %v", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("fd %d: CLOEXEC not set after markCloexecAll", fd)
		}
	}
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

// TestCheckControlFD verifies the pre-handoff validation accepts the
// SOCK_SEQPACKET control socketpair and rejects the failure modes that
// would otherwise surface only deep inside sendListener as an opaque
// "socket operation on non-socket": an fd that is open but not a
// socket (e.g. an exec run without --preserve-fds), and a socket of the
// wrong type.
func TestCheckControlFD(t *testing.T) {
	t.Run("seqpacket socketpair is accepted", func(t *testing.T) {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
		if err != nil {
			t.Fatalf("socketpair: %v", err)
		}
		defer unix.Close(pair[0])
		defer unix.Close(pair[1])
		if err := checkControlFD(pair[0]); err != nil {
			t.Fatalf("checkControlFD(%d) = %v, want nil", pair[0], err)
		}
	})

	t.Run("stream socket is rejected", func(t *testing.T) {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		defer unix.Close(fd)
		err = checkControlFD(fd)
		if err == nil {
			t.Fatal("checkControlFD(stream fd) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "SOCK_STREAM") {
			t.Errorf("error %q does not name SOCK_STREAM", err)
		}
	})

	t.Run("dgram socket is rejected", func(t *testing.T) {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		defer unix.Close(fd)
		err = checkControlFD(fd)
		if err == nil {
			t.Fatal("checkControlFD(dgram fd) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "SOCK_DGRAM") {
			t.Errorf("error %q does not name SOCK_DGRAM", err)
		}
	})

	t.Run("regular file is rejected", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "fd")
		if err != nil {
			t.Fatalf("createtemp: %v", err)
		}
		defer f.Close()
		fd := int(f.Fd())
		err = checkControlFD(fd)
		if err == nil {
			t.Fatal("checkControlFD(file fd) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "not a socket") {
			t.Errorf("error %q does not say the fd is not a socket", err)
		}
	})

	t.Run("closed fd is rejected", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "fd")
		if err != nil {
			t.Fatalf("createtemp: %v", err)
		}
		fd := int(f.Fd())
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := checkControlFD(fd); err == nil {
			t.Fatal("checkControlFD(closed fd) = nil, want an error")
		}
	})
}

// TestRunChildForwardsSIGTERM verifies that SIGTERM delivered to the broker
// reaches the child, and that the child's exit status is still returned
// (so the caller's cleanup runs).
func TestRunChildForwardsSIGTERM(t *testing.T) {
	_, broker := newControlSocketpair(t)
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = unix.Kill(os.Getpid(), unix.SIGTERM)
	}()
	script := `trap 'exit 9' TERM; while :; do sleep 0.05; done`
	if got := runChild([]string{"sh", "-c", script}, t.TempDir()+"/s.sock", broker); got != 9 {
		t.Fatalf("runChild: got exit code %d, want 9", got)
	}
}

// TestWaitForHostSIGTERM verifies that standalone mode returns cleanly on
// SIGTERM instead of dying with the socket left behind.
func TestWaitForHostSIGTERM(t *testing.T) {
	_, broker := newControlSocketpair(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = unix.Kill(os.Getpid(), unix.SIGTERM)
	}()
	done := make(chan int, 1)
	go func() { done <- waitForHost(broker) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("waitForHost: got %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForHost did not return after SIGTERM")
	}
}
