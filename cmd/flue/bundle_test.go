package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestBrokerBundleRoundTrip verifies the core guarantee: the file written
// by broker-bundle is byte-identical to the embed source on disk
// (cmd/flue/flue-broker.sigstore), and is a regular non-executable file
// (mode 0644).
func TestBrokerBundleRoundTrip(t *testing.T) {
	want, err := os.ReadFile("flue-broker.sigstore")
	if err != nil {
		t.Fatalf("read embed source: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flue-broker.sigstore")
	if err := writeEmbeddedBundle(path); err != nil {
		t.Fatalf("writeEmbeddedBundle(%q): %v", path, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("written bytes differ from the embed source (%d vs %d bytes)", len(got), len(want))
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 0644", perm)
	}
}

// TestBrokerBundleOverwrite verifies that writing to an existing file
// replaces it (a normal re-extract), ending with the identical bytes
// again.
func TestBrokerBundleOverwrite(t *testing.T) {
	want, err := os.ReadFile("flue-broker.sigstore")
	if err != nil {
		t.Fatalf("read embed source: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flue-broker.sigstore")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	if err := writeEmbeddedBundle(path); err != nil {
		t.Fatalf("writeEmbeddedBundle (overwrite): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("overwritten bytes differ from the embed source")
	}
}

// TestWriteEmbeddedBundleErrors covers the failure modes: a directory as
// target, and a target whose directory does not exist.
func TestWriteEmbeddedBundleErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("target is a directory", func(t *testing.T) {
		if err := writeEmbeddedBundle(dir); err == nil {
			t.Fatalf("writeEmbeddedBundle(dir) succeeded, want error")
		}
	})

	t.Run("directory does not exist", func(t *testing.T) {
		path := filepath.Join(dir, "no-such-dir", "flue-broker.sigstore")
		if err := writeEmbeddedBundle(path); err == nil {
			t.Fatalf("writeEmbeddedBundle(missing dir) succeeded, want error")
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("no-such-dir appeared: %v", err)
		}
	})
}

// TestIsPlaceholderBundle is the table for the placeholder-detection
// heuristic: the committed placeholder template is recognized, while a
// real-looking bundle (a different kind), unparsable bytes, and the empty
// input are all treated as real bundles (cosign is the authority on what
// a real one is — this only gates the warning).
func TestIsPlaceholderBundle(t *testing.T) {
	ph, err := os.ReadFile("flue-broker.sigstore.placeholder")
	if err != nil {
		t.Fatalf("read placeholder template: %v", err)
	}
	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{"placeholder template", ph, true},
		{"placeholder without note", []byte(`{"kind":"FlueLocalPlaceholder"}`), true},
		{"real bundle kind", []byte(`{"apiVersion":"v0.3","kind":"Bundle","verificationMaterial":{}}`), false},
		{"no kind field", []byte(`{"foo":"bar"}`), false},
		{"unparsable", []byte(`not json`), false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlaceholderBundle(tt.in); got != tt.want {
				t.Errorf("isPlaceholderBundle = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCobraBrokerBundle runs the broker-bundle subcommand through the
// root command and checks the written file: it must be byte-identical to
// the embed source file. The subcommand carries no needs-agent
// annotation, so executing it needs no agent socket.
func TestCobraBrokerBundle(t *testing.T) {
	data, err := os.ReadFile("flue-broker.sigstore")
	if err != nil {
		t.Fatalf("read embed source: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flue-broker.sigstore")
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"broker-bundle", path})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute([broker-bundle]) = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("written bytes differ from the embed source (%d vs %d bytes)", len(got), len(data))
	}
}

// TestNeedsAgentAnnotations includes broker-bundle: like extract-broker,
// it works without SSH_AUTH_SOCK being set (the root PersistentPreRun
// must not resolve the agent socket for it).
func TestBrokerBundleNoAgentAnnotation(t *testing.T) {
	if brokerBundleCmd.Annotations[needsAgentAnnotation] != "" {
		t.Error("broker-bundle must not be annotated as needing the agent socket")
	}
}

// TestBrokerBundlePreRunWithoutAgent verifies the root PersistentPreRun
// does not require an agent socket for broker-bundle (a regression here
// would os.Exit in fatalf and fail the test binary).
func TestBrokerBundlePreRunWithoutAgent(t *testing.T) {
	old, had := os.LookupEnv("SSH_AUTH_SOCK")
	os.Unsetenv("SSH_AUTH_SOCK")
	t.Cleanup(func() {
		if had {
			os.Setenv("SSH_AUTH_SOCK", old)
		}
	})
	sockArg = ""
	rootCmd.PersistentPreRun(brokerBundleCmd, nil)
}
