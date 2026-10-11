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

// TestEnvBool guards the truthy-string parsing used for boolean env-var
// fallbacks: the accepted truthy set (case-insensitive, surrounding
// whitespace trimmed) is true, everything else is false.
func TestEnvBool(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"1", true}, {"t", true}, {"true", true}, {"y", true},
		{"yes", true}, {"on", true},
		{"TRUE", true}, {" Yes ", true}, {"1 ", true},
		{"0", false}, {"false", false}, {"no", false}, {"n", false},
		{"off", false}, {"", false}, {"abc", false},
	} {
		if got := envBool(tc.in); got != tc.want {
			t.Errorf("envBool(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestInjectBrokerEffective guards the --inject-broker resolution: an
// explicitly set flag wins, else the FLUE_INJECT_BROKER environment
// variable, else false. The enter command's flag set is package-global, so
// the test mutates its Changed state and the bound var, restoring both
// afterwards.
func TestInjectBrokerEffective(t *testing.T) {
	f := enterCmd.Flags().Lookup("inject-broker")
	if f == nil {
		t.Fatal("inject-broker flag is not registered on enter")
	}
	oldChanged, oldVar := f.Changed, enterInjectBroker
	t.Cleanup(func() { f.Changed, enterInjectBroker = oldChanged, oldVar })

	for _, tc := range []struct {
		name    string
		changed bool
		flagVal bool
		env     string
		want    bool
	}{
		{"neither set", false, false, "", false},
		{"env true", false, false, "1", true},
		{"env truthy (yes)", false, false, "yes", true},
		{"env falsy", false, false, "false", false},
		{"flag true beats env false", true, true, "false", true},
		{"flag false beats env true", true, false, "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.Changed, enterInjectBroker = tc.changed, tc.flagVal
			t.Setenv("FLUE_INJECT_BROKER", tc.env)
			if got := injectBrokerEffective(enterCmd); got != tc.want {
				t.Errorf("injectBrokerEffective() = %v, want %v", got, tc.want)
			}
		})
	}
}
