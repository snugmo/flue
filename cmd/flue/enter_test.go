package main

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestEnterTarget verifies how `flue enter` arguments are split into the
// container name and the in-container command: the snugmo container
// default, a nil command (the broker resolves a shell in the container)
// when no command is given, and an explicit command passed through.
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
			name:          "no args defaults to snugmo and a broker-resolved shell",
			args:          nil,
			wantContainer: "snugmo",
			wantCmd:       nil,
		},
		{
			name:          "container only leaves the command to the broker",
			args:          []string{"c"},
			wantContainer: "c",
			wantCmd:       nil,
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
			container, cmd, err := enterTarget(tt.args)
			if err != nil {
				t.Fatalf("enterTarget(%v) error: %v", tt.args, err)
			}
			if container != tt.wantContainer {
				t.Errorf("container = %q, want %q", container, tt.wantContainer)
			}
			if diff := cmp.Diff(tt.wantCmd, cmd); diff != "" {
				t.Errorf("cmd mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEnterTargetDashDash verifies the `--` separator: the tokens after it
// are the command, the single positional argument before it (if any) names
// the container, a trailing separator is a no-op, and more than one
// positional before it is a usage error. The separator itself is stripped
// by pflag before Run, so each case installs the matching raw process
// arguments (what enterDashDash reads from os.Args) and the positional
// arguments Run would receive.
func TestEnterTargetDashDash(t *testing.T) {
	t.Setenv("FLUE_ENTER_CMD", "")

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	for _, tt := range []struct {
		name          string
		raw           []string // process args after the program name
		cobraArgs     []string // positionals Run receives (-- stripped)
		wantContainer string
		wantCmd       []string
		wantErr       bool
	}{
		{
			name:          "separator before any positional runs the command in the default container",
			raw:           []string{"enter", "--", "bash", "-l"},
			cobraArgs:     []string{"bash", "-l"},
			wantContainer: "snugmo",
			wantCmd:       []string{"bash", "-l"},
		},
		{
			name:          "a flag with a value before the separator is not a container",
			raw:           []string{"enter", "--log-level", "debug", "--", "bash"},
			cobraArgs:     []string{"bash"},
			wantContainer: "snugmo",
			wantCmd:       []string{"bash"},
		},
		{
			name:          "a container before the separator is honored",
			raw:           []string{"enter", "c", "--", "bash", "-l"},
			cobraArgs:     []string{"c", "bash", "-l"},
			wantContainer: "c",
			wantCmd:       []string{"bash", "-l"},
		},
		{
			name:          "a trailing separator is a no-op",
			raw:           []string{"enter", "--"},
			cobraArgs:     nil,
			wantContainer: "snugmo",
			wantCmd:       nil,
		},
		{
			name:          "a trailing separator after a container is a no-op",
			raw:           []string{"enter", "c", "--"},
			cobraArgs:     []string{"c"},
			wantContainer: "c",
			wantCmd:       nil,
		},
		{
			name:      "more than one positional before the separator is a usage error",
			raw:       []string{"enter", "a", "b", "--", "sh"},
			cobraArgs: []string{"a", "b", "sh"},
			wantErr:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			os.Args = append([]string{"flue"}, tt.raw...)
			container, cmd, err := enterTarget(tt.cobraArgs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("enterTarget(%v) = (%q, %v), want an error", tt.cobraArgs, container, cmd)
				}
				return
			}
			if err != nil {
				t.Fatalf("enterTarget(%v) error: %v", tt.cobraArgs, err)
			}
			if container != tt.wantContainer {
				t.Errorf("container = %q, want %q", container, tt.wantContainer)
			}
			if diff := cmp.Diff(tt.wantCmd, cmd); diff != "" {
				t.Errorf("cmd mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEnterDefaultCmd verifies the default command resolution: the
// whitespace-split value of $FLUE_ENTER_CMD when set and non-blank, else
// nil (the broker resolves a shell in the container, via -default-shell).
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
		if got := enterDefaultCmd(); got != nil {
			t.Errorf("enterDefaultCmd() = %v, want nil (broker resolves the shell)", got)
		}
	})

	for _, tt := range []struct {
		name string
		env  string
		want []string
	}{
		{"blank", "", nil},
		{"whitespace only", "   ", nil},
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
// enter mode: the tty/non-tty variants, the flue-broker wrapper with the
// command appended (no -e — the broker sets SSH_AUTH_SOCK itself and
// generates its per-session socket path), and the -default-shell flag
// for a nil command (the broker resolves a shell in the container),
// including the injected-broker form where the broker is exec'd from
// /proc/self/fd/4.
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

	t.Run("nil cmd passes -default-shell", func(t *testing.T) {
		got := enterExecArgs("c", nil, true, false)
		want := []string{
			"exec", "-ti",
			"--preserve-fds", "1",
			"c", "flue-broker", "-default-shell",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("default-shell args (-want +got):\n%s", diff)
		}
	})

	t.Run("nil cmd with injection", func(t *testing.T) {
		got := enterExecArgs("c", nil, true, true)
		want := []string{
			"exec", "-ti",
			"--preserve-fds", "2",
			"c", "/proc/self/fd/4", "-default-shell",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("default-shell injected args (-want +got):\n%s", diff)
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
