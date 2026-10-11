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
