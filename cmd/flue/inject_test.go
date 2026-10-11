package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// TestInjectBrokerMemfdContent verifies that injectBrokerMemfd produces a memfd
// whose bytes are identical to the embedded flue-broker (the same guarantee as
// extract-broker), and that it is a real memfd carrying the expected name.
func TestInjectBrokerMemfdContent(t *testing.T) {
	if !looksLikeELF(flueBrokerBin) {
		t.Skip("embedded broker is not an ELF (build flue-broker first)")
	}
	f, err := injectBrokerMemfd()
	if err != nil {
		t.Fatalf("injectBrokerMemfd: %v", err)
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek memfd: %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read memfd: %v", err)
	}
	if !bytes.Equal(got, flueBrokerBin) {
		t.Fatalf("memfd content != embedded broker (got %d bytes, want %d)", len(got), len(flueBrokerBin))
	}

	// A memfd's /proc/self/fd/<n> target is "memfd:<name> (deleted)"; confirm
	// the name so a regression to a plain anonymous fd is caught.
	link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		t.Fatalf("readlink /proc/self/fd: %v", err)
	}
	if !strings.Contains(link, "memfd:"+brokerMemfdName) {
		t.Errorf("memfd link = %q, want it to contain %q", link, "memfd:"+brokerMemfdName)
	}
}

// TestInjectBrokerMemfdFailsOnBadELF verifies the guard that refuses to build
// a memfd from a non-ELF embedded blob (the symptom of building flue without
// building flue-broker first). flueBrokerBin is restored after the test.
func TestInjectBrokerMemfdFailsOnBadELF(t *testing.T) {
	orig := flueBrokerBin
	flueBrokerBin = []byte("definitely not an ELF")
	defer func() { flueBrokerBin = orig }()

	if _, err := injectBrokerMemfd(); err == nil {
		t.Fatal("injectBrokerMemfd with a non-ELF blob succeeded, want error")
	}
}

// TestEnterExecArgsInjectBroker verifies the podman exec argument list for
// --inject-broker: two preserved fds (fd 3 control, fd 4 memfd) and the
// broker invoked via /proc/self/fd/4 instead of the image's flue-broker.
func TestEnterExecArgsInjectBroker(t *testing.T) {
	t.Run("tty", func(t *testing.T) {
		got := enterExecArgs("c", []string{"/bin/sh"}, true, true)
		want := []string{
			"exec", "-ti",
			"--preserve-fds", "2",
			"c", "/proc/self/fd/4", "/bin/sh",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("inject tty args (-want +got):\n%s", diff)
		}
	})
	t.Run("non-tty", func(t *testing.T) {
		got := enterExecArgs("c", []string{"bash", "-c", "echo hi"}, false, true)
		want := []string{
			"exec", "-i",
			"--preserve-fds", "2",
			"c", "/proc/self/fd/4", "bash", "-c", "echo hi",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("inject non-tty args (-want +got):\n%s", diff)
		}
	})
}

// TestInjectBrokerFlagRegistered guards that --inject-broker is wired as a
// (non-persistent, enter-scoped) default-on bool flag, with the negative
// form --no-inject-broker registered alongside it (pflag does not generate
// --no-<flag> forms itself, so flue registers it explicitly).
func TestInjectBrokerFlagRegistered(t *testing.T) {
	fl := enterCmd.Flags()
	if f := fl.Lookup("inject-broker"); f == nil {
		t.Fatal("--inject-broker flag is not registered on enter")
	} else if f.DefValue != "true" {
		t.Errorf("--inject-broker default = %q, want true", f.DefValue)
	}
	if f := fl.Lookup("no-inject-broker"); f == nil {
		t.Fatal("--no-inject-broker is not registered on enter")
	} else if f.NoOptDefVal != "true" {
		t.Errorf("--no-inject-broker NoOptDefVal = %q, want %q (bare flag must disable without consuming an argument)",
			f.NoOptDefVal, "true")
	}
}

// TestNegInjectBrokerFlags parses the two disabling forms at the pflag
// level: --no-inject-broker (a registered companion flag) and
// --inject-broker=false (pflag's standard form for any bool flag). Both
// must leave the bound variable false without consuming a following
// argument, and a plain --inject-broker re-enables.
func TestNegInjectBrokerFlags(t *testing.T) {
	fl := enterCmd.Flags()
	oldVar := enterInjectBroker
	oldChanged := map[string]bool{}
	for _, name := range []string{"inject-broker", "no-inject-broker"} {
		if f := fl.Lookup(name); f != nil {
			oldChanged[name] = f.Changed
		}
	}
	t.Cleanup(func() {
		enterInjectBroker = oldVar
		for name, c := range oldChanged {
			fl.Lookup(name).Changed = c
		}
	})

	if err := fl.Parse([]string{"--no-inject-broker"}); err != nil {
		t.Fatalf("parse --no-inject-broker: %v", err)
	}
	if enterInjectBroker {
		t.Error("after --no-inject-broker: want injection disabled")
	}

	if err := fl.Parse([]string{"--inject-broker"}); err != nil {
		t.Fatalf("re-enable: parse --inject-broker: %v", err)
	}
	if !enterInjectBroker {
		t.Error("after --inject-broker: want injection enabled")
	}

	if err := fl.Parse([]string{"--inject-broker=false"}); err != nil {
		t.Fatalf("parse --inject-broker=false: %v", err)
	}
	if enterInjectBroker {
		t.Error("after --inject-broker=false: want injection disabled")
	}
}

// TestInjectBrokerMemfdSealed verifies that the memfd handed to the
// container cannot be modified or resized.
func TestInjectBrokerMemfdSealed(t *testing.T) {
	if !looksLikeELF(flueBrokerBin) {
		t.Skip("embedded broker is not an ELF (build flue-broker first)")
	}
	f, err := injectBrokerMemfd()
	if err != nil {
		t.Fatalf("injectBrokerMemfd: %v", err)
	}
	defer f.Close()

	if _, err := f.WriteAt([]byte{0}, 0); err == nil {
		t.Fatal("write to sealed memfd succeeded")
	}
	if err := f.Truncate(0); err == nil {
		t.Fatal("truncate of sealed memfd succeeded")
	}
}

// TestInjectBrokerMemfdExecutable verifies that the sealed memfd can still
// be exec'd through /proc/self/fd/<n>, which is how the container runs it.
func TestInjectBrokerMemfdExecutable(t *testing.T) {
	if !looksLikeELF(flueBrokerBin) {
		t.Skip("embedded broker is not an ELF (build flue-broker first)")
	}
	f, err := injectBrokerMemfd()
	if err != nil {
		t.Fatalf("injectBrokerMemfd: %v", err)
	}
	defer f.Close()

	cmd := exec.Command("/proc/self/fd/3", "-log-level", "bogus")
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !strings.Contains(string(out), "unknown log level") {
		t.Fatalf("exec of memfd broker: err=%v out=%q (want the broker's own flag error)", err, out)
	}
}

// TestMemfdCreateWithFallback pins the MFD_EXEC fallback policy: EINVAL
// (kernel older than 6.3, the flag unknown) triggers exactly one retry
// without MFD_EXEC; any other failure — in particular EACCES from
// vm.memfd_noexec=2, which refuses executable memfds — is not retried and
// yields an actionable error. The create call is a closure so the test
// simulates both failures without a specially configured kernel.
func TestMemfdCreateWithFallback(t *testing.T) {
	t.Run("EINVAL falls back once, without MFD_EXEC", func(t *testing.T) {
		var calls []int
		create := func(flags int) (int, error) {
			calls = append(calls, flags)
			if flags&unix.MFD_EXEC != 0 {
				return 0, unix.EINVAL
			}
			return 7, nil
		}
		fd, err := memfdCreateWithFallback(create)
		if err != nil {
			t.Fatalf("memfdCreateWithFallback: %v", err)
		}
		if fd != 7 {
			t.Fatalf("fd = %d, want 7", fd)
		}
		want := []int{brokerMemfdFlags, unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING}
		if diff := cmp.Diff(want, calls); diff != "" {
			t.Fatalf("create calls (-want +got):\n%s", diff)
		}
	})

	t.Run("EACCES is not retried and names the fix", func(t *testing.T) {
		var calls int
		create := func(flags int) (int, error) {
			calls++
			return 0, unix.EACCES
		}
		_, err := memfdCreateWithFallback(create)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if calls != 1 {
			t.Fatalf("create called %d times, want 1 (no fallback retry on EACCES)", calls)
		}
		msg := err.Error()
		for _, want := range []string{"vm.memfd_noexec", "--no-inject-broker"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not mention %q", msg, want)
			}
		}
		if !errors.Is(err, unix.EACCES) {
			t.Errorf("error %v does not wrap EACCES", err)
		}
	})

	t.Run("first success is used as-is", func(t *testing.T) {
		var calls int
		create := func(flags int) (int, error) {
			calls++
			return 3, nil
		}
		fd, err := memfdCreateWithFallback(create)
		if err != nil {
			t.Fatalf("memfdCreateWithFallback: %v", err)
		}
		if fd != 3 || calls != 1 {
			t.Fatalf("fd = %d, calls = %d, want fd 3 and one call", fd, calls)
		}
	})
}
