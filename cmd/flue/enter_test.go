package main

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestEnterTarget verifies how `flue enter` arguments are split into the
// container name and the in-container command, including the snugmo
// container and /bin/sh defaults when no arguments are given.
func TestEnterTarget(t *testing.T) {
	// Keep the default-command resolution deterministic regardless of the
	// test environment (enterTarget consults $FLUE_ENTER_CMD).
	t.Setenv("FLUE_ENTER_CMD", "")

	tests := []struct {
		name          string
		args          []string
		wantContainer string
		wantCmd       []string
	}{
		{
			name:          "no args defaults to snugmo and /bin/sh",
			args:          nil,
			wantContainer: "snugmo",
			wantCmd:       []string{"/bin/sh"},
		},
		{
			name:          "container only defaults to /bin/sh",
			args:          []string{"c"},
			wantContainer: "c",
			wantCmd:       []string{"/bin/sh"},
		},
		{
			name:          "command is passed through",
			args:          []string{"c", "bash", "-l"},
			wantContainer: "c",
			wantCmd:       []string{"bash", "-l"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container, cmd := enterTarget(tt.args)
			if container != tt.wantContainer {
				t.Errorf("container = %q, want %q", container, tt.wantContainer)
			}
			if diff := cmp.Diff(tt.wantCmd, cmd); diff != "" {
				t.Errorf("cmd mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEnterDefaultCmd verifies the default command resolution: /bin/sh
// when $FLUE_ENTER_CMD is unset, blank, or whitespace-only, and the
// whitespace-split value of the variable otherwise.
func TestEnterDefaultCmd(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		prev, had := os.LookupEnv("FLUE_ENTER_CMD")
		os.Unsetenv("FLUE_ENTER_CMD")
		defer func() {
			if had {
				os.Setenv("FLUE_ENTER_CMD", prev)
			} else {
				os.Unsetenv("FLUE_ENTER_CMD")
			}
		}()
		if got := enterDefaultCmd(); !cmp.Equal(got, []string{"/bin/sh"}) {
			t.Errorf("enterDefaultCmd() = %v, want [/bin/sh]", got)
		}
	})

	for _, tt := range []struct {
		name string
		env  string
		want []string
	}{
		{"blank", "", []string{"/bin/sh"}},
		{"whitespace only", "   ", []string{"/bin/sh"}},
		{"single word", "bash", []string{"bash"}},
		{"word and args", "bash -l", []string{"bash", "-l"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FLUE_ENTER_CMD", tt.env)
			if got := enterDefaultCmd(); !cmp.Equal(got, tt.want) {
				t.Errorf("enterDefaultCmd() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEnterExecArgs verifies the podman exec argument list built for
// enter mode: the tty/non-tty variants, the fixed SSH_AUTH_SOCK, and the
// flue-broker wrapper with the command appended.
func TestEnterExecArgs(t *testing.T) {
	t.Run("tty", func(t *testing.T) {
		got := enterExecArgs("c", []string{"/bin/sh"}, true, false)
		want := []string{
			"exec", "-ti",
			"--preserve-fds", "1",
			"c", "flue-broker", "/bin/sh",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("tty args (-want +got):\n%s", diff)
		}
	})

	t.Run("non-tty", func(t *testing.T) {
		got := enterExecArgs("c", []string{"bash", "-c", "echo hi"}, false, false)
		want := []string{
			"exec", "-i",
			"--preserve-fds", "1",
			"c", "flue-broker", "bash", "-c", "echo hi",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("non-tty args (-want +got):\n%s", diff)
		}
	})
}

// TestParseSessionCheckMode verifies the --session-check value
// mapping: the three recognized values map to their modes, anything
// else errors.
func TestParseSessionCheckMode(t *testing.T) {
	good := map[string]sessionCheckMode{
		"required": sessionCheckRequired,
		"advisory": sessionCheckAdvisory,
		"off":      sessionCheckOff,
	}
	for s, want := range good {
		if got, err := parseSessionCheckMode(s); err != nil || got != want {
			t.Errorf("parseSessionCheckMode(%q) = (%v, %v), want (%v, nil)", s, got, err, want)
		}
	}
	for _, s := range []string{"", "on", "REQUIRED", "advisory "} {
		if got, err := parseSessionCheckMode(s); err == nil {
			t.Errorf("parseSessionCheckMode(%q) succeeded (%v), want an error", s, got)
		}
	}
}
