package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestEnterTarget verifies how `flue enter` arguments are split into the
// container name and the in-container command, including the /bin/sh
// default when no command is given.
func TestEnterTarget(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantContainer string
		wantCmd       []string
		wantErr       bool
	}{
		{
			name:    "no args is an error",
			args:    nil,
			wantErr: true,
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
			container, cmd, err := enterTarget(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("enterTarget(%v) succeeded, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("enterTarget(%v): %v", tt.args, err)
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
