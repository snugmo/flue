package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestCobraSubcommands guards the CLI surface: the two subcommands must be
// registered on the root with their expected names and use-lines. This
// catches accidental renames (e.g. dropping the exec subcommand or retyping
// a use-line) that the pure-function tests (enterTarget, enterExecArgs) do
// not cover.
func TestCobraSubcommands(t *testing.T) {
	byName := map[string]*cobra.Command{}
	for _, c := range rootCmd.Commands() {
		byName[c.Name()] = c
	}
	for name, wantUse := range map[string]string{
		"exec":           "exec [command [args...]]",
		"enter":          "enter <container> [command [args...]]",
		"extract-broker": "extract-broker <filename>",
	} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("subcommand %q not registered on the root", name)
		}
		if c.Use != wantUse {
			t.Errorf("subcommand %q: Use = %q, want %q", name, c.Use, wantUse)
		}
	}
}

// TestCobraPersistentFlags guards that the three global flags are registered
// as *persistent* flags on the root, so they are inherited by both subcommands
// and can be placed before the subcommand (the current flue convention).
func TestCobraPersistentFlags(t *testing.T) {
	for _, name := range []string{"sock", "log-level", "allow-key"} {
		if rootCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("persistent flag --%s is not registered on the root", name)
		}
	}
}
