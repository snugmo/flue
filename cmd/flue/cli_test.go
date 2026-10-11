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
		"enter":          "enter [container] [command [args...]]",
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

// TestResolveLogLevel guards the --log-level resolution: an explicit flag
// wins, else the FLUE_LOG_LEVEL environment variable, else the built-in
// default "info".
func TestResolveLogLevel(t *testing.T) {
	old := logLevel
	t.Cleanup(func() { logLevel = old })

	for _, tc := range []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"explicit flag wins", "debug", "", "debug"},
		{"explicit flag wins over env", "warn", "error", "warn"},
		{"env fills in when flag unset", "", "error", "error"},
		{"default when neither set", "", "", "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logLevel = tc.flag
			t.Setenv("FLUE_LOG_LEVEL", tc.env)
			if got := resolveLogLevel(); got != tc.want {
				t.Errorf("resolveLogLevel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEnvBool guards the tri-state env-var parser: recognized truthy and
// falsy values are distinguished (case-insensitive, surrounding whitespace
// trimmed), and everything else — including empty and nonsense strings —
// reports "not recognized" so the caller falls back to its default.
func TestEnvBool(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  bool
		known bool
	}{
		{"1", true, true}, {"t", true, true}, {"true", true, true},
		{"y", true, true}, {"yes", true, true}, {"on", true, true},
		{"TRUE", true, true}, {" Yes ", true, true}, {"1 ", true, true},
		{"0", false, true}, {"f", false, true}, {"false", false, true},
		{"no", false, true}, {"n", false, true}, {"off", false, true},
		{"", false, false}, {"abc", false, false},
	} {
		got, known := envBool(tc.in)
		if got != tc.want || known != tc.known {
			t.Errorf("envBool(%q) = (%v, %v), want (%v, %v)",
				tc.in, got, known, tc.want, tc.known)
		}
	}
}

// TestInjectBrokerEffective guards the default-on --inject-broker
// resolution: an explicitly set flag (either form) wins, else a recognized
// FLUE_INJECT_BROKER value, else the default (on). The enter command's
// flag set is package-global, so the test mutates the Changed state and
// the bound var, restoring both afterwards.
func TestInjectBrokerEffective(t *testing.T) {
	fIB := enterCmd.Flags().Lookup("inject-broker")
	fNI := enterCmd.Flags().Lookup("no-inject-broker")
	if fIB == nil || fNI == nil {
		t.Fatal("inject-broker / no-inject-broker flags not registered on enter")
	}
	oldChangedIB, oldChangedNI := fIB.Changed, fNI.Changed
	oldVar := enterInjectBroker
	t.Cleanup(func() {
		fIB.Changed, fNI.Changed, enterInjectBroker = oldChangedIB, oldChangedNI, oldVar
	})

	for _, tc := range []struct {
		name      string
		ibChanged bool // --inject-broker set on the command line
		niChanged bool // --no-inject-broker set on the command line
		varVal    bool // the bound variable after parsing
		env       string
		want      bool
	}{
		{"default on (nothing set)", false, false, true, "", true},
		{"env truthy", false, false, true, "1", true},
		{"env truthy (yes)", false, false, true, "yes", true},
		{"env falsy disables (0)", false, false, true, "0", false},
		{"env falsy disables (off)", false, false, true, "off", false},
		{"env unrecognized keeps default", false, false, true, "banana", true},
		{"--inject-broker beats env off", true, false, true, "0", true},
		{"--inject-broker=false beats env on", true, false, false, "1", false},
		{"--no-inject-broker beats env on", false, true, false, "1", false},
		{"--no-inject-broker beats env off", false, true, false, "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fIB.Changed, fNI.Changed, enterInjectBroker = tc.ibChanged, tc.niChanged, tc.varVal
			t.Setenv("FLUE_INJECT_BROKER", tc.env)
			if got := injectBrokerEffective(enterCmd); got != tc.want {
				t.Errorf("injectBrokerEffective() = %v, want %v", got, tc.want)
			}
		})
	}
}
