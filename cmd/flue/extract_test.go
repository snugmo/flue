package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// TestExtractBrokerRoundTrip verifies the core guarantee: the file
// written by broker executable is byte-identical to the embed source on
// disk (cmd/flue/flue-broker.bin — the built flue-broker), and is
// executable (mode 0755).
func TestExtractBrokerRoundTrip(t *testing.T) {
	want, err := os.ReadFile("flue-broker.bin")
	if err != nil {
		t.Fatalf("read embed source: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flue-broker")
	if err := writeEmbeddedBroker(path); err != nil {
		t.Fatalf("writeEmbeddedBroker(%q): %v", path, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("extracted bytes differ from the embed source (%d vs %d bytes)", len(got), len(want))
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat extracted file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o755 {
		t.Errorf("mode = %o, want 0755", perm)
	}
}

// TestExtractBrokerOverwrite verifies that extracting to an existing
// file replaces it (a normal re-extract), ending with the identical
// bytes again.
func TestExtractBrokerOverwrite(t *testing.T) {
	want, err := os.ReadFile("flue-broker.bin")
	if err != nil {
		t.Fatalf("read embed source: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flue-broker")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	if err := writeEmbeddedBroker(path); err != nil {
		t.Fatalf("writeEmbeddedBroker (overwrite): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("overwritten bytes differ from the embed source")
	}
}

// TestExtractBrokerErrors covers the failure modes: a directory as
// target, and a target whose directory does not exist.
func TestExtractBrokerErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("target is a directory", func(t *testing.T) {
		if err := writeEmbeddedBroker(dir); err == nil {
			t.Fatalf("writeEmbeddedBroker(dir) succeeded, want error")
		}
	})

	t.Run("directory does not exist", func(t *testing.T) {
		path := filepath.Join(dir, "no-such-dir", "flue-broker")
		if err := writeEmbeddedBroker(path); err == nil {
			t.Fatalf("writeEmbeddedBroker(missing dir) succeeded, want error")
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("no-such-dir appeared: %v", err)
		}
	})
}

// TestLooksLikeELF is the table for the ELF-magic sanity check.
func TestLooksLikeELF(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{"ELF magic", []byte{0x7f, 'E', 'L', 'F'}, true},
		{"ELF with more", append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 16)...), true},
		{"too short", []byte{0x7f, 'E', 'L'}, false},
		{"wrong magic", []byte("hello world"), false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeELF(tt.in); got != tt.want {
				t.Errorf("looksLikeELF = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEmbeddedBrokerIsELF guards the build order at test time: it fails
// if this flue was built with a non-broker embed source (a stub, or a
// missing build step that some tool substituted), so the failure surfaces
// here rather than in a container later.
func TestEmbeddedBrokerIsELF(t *testing.T) {
	if !looksLikeELF(flueBrokerBin) {
		t.Fatal("embedded flue-broker is not a Linux ELF binary: build cmd/flue-broker (to cmd/flue/flue-broker.bin) before building cmd/flue, then re-run the tests")
	}
}

// TestNeedsAgentAnnotations guards which subcommands make the root
// PersistentPreRun resolve the agent socket: exec and enter must,
// the broker sub-subcommands must not (they work without SSH_AUTH_SOCK).
func TestNeedsAgentAnnotations(t *testing.T) {
	for _, c := range []*cobra.Command{execCmd, enterCmd} {
		if c.Annotations[needsAgentAnnotation] == "" {
			t.Errorf("subcommand %q is missing the needs-agent annotation", c.Name())
		}
	}
	if brokerExecutableCmd.Annotations[needsAgentAnnotation] != "" {
		t.Error("broker executable must not be annotated as needing the agent socket")
	}
}

// TestExtractBrokerPreRunWithoutAgent verifies the root PersistentPreRun
// does not require an agent socket for broker executable (a regression
// here would os.Exit in fatalf and fail the test binary).
func TestExtractBrokerPreRunWithoutAgent(t *testing.T) {
	old, had := os.LookupEnv("SSH_AUTH_SOCK")
	os.Unsetenv("SSH_AUTH_SOCK")
	t.Cleanup(func() {
		if had {
			os.Setenv("SSH_AUTH_SOCK", old)
		}
	})
	sockArg = ""
	rootCmd.PersistentPreRun(brokerExecutableCmd, nil)
}
