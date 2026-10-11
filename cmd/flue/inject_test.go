package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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
			"-e", "SSH_AUTH_SOCK=" + enterSocket,
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
			"-e", "SSH_AUTH_SOCK=" + enterSocket,
			"c", "/proc/self/fd/4", "bash", "-c", "echo hi",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("inject non-tty args (-want +got):\n%s", diff)
		}
	})
}

// TestInjectBrokerFlagRegistered guards that --inject-broker is wired as a
// (non-persistent, enter-scoped) flag, so a rename or accidental removal of
// the binding is caught by the pure-function suite.
func TestInjectBrokerFlagRegistered(t *testing.T) {
	if f := enterCmd.Flags().Lookup("inject-broker"); f == nil {
		t.Fatal("--inject-broker flag is not registered on enter")
	} else if f.DefValue != "false" {
		t.Errorf("--inject-broker default = %q, want false", f.DefValue)
	}
}
