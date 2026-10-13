// Command flue is the host-side half of the flue SSH agent bridge.
//
// It creates a Unix socketpair (SOCK_SEQPACKET) and passes one end into the
// container as fd 3 via `podman exec --preserve-fds 1`. The container-side
// broker (flue-broker) listens on a Unix socket, then hands that listening
// fd back to the host over the control channel. The host validates the
// received fd (a listening Unix stream socket) and accepts client
// connections directly on it, proxying each one to the real host agent
// ($SSH_AUTH_SOCK) using golang.org/x/crypto/ssh/agent with a filtering
// wrapper. Each client gets a fresh upstream connection, so repeated
// ssh / ssh-add / git invocations all work.
//
// Lifetime is driven by the process tree, not the control channel. The
// control channel carries exactly one message (the listener fd); its
// connection stays open until the broker is gone (so the broker never sees a
// premature "host gone"), and flue blocks on its child (the `podman exec`
// that runs the broker) exiting. In standalone mode the broker runs until
// flue is signalled; in wrapper mode the broker exits when the wrapped
// command does, so flue returns that command's exit status.
//
// The filter implements agent.ExtendedAgent:
//   - List: forwarded, but only keys allowed by the --allow-key and
//     --allow-key-type lists are returned (all keys if neither flag is
//     given).
//   - Sign / SignWithFlags: forwarded only for keys allowed by both
//     lists.
//   - Add, Remove, RemoveAll, Lock, Unlock, Signers: always denied.
//   - Extension: only session-bind@openssh.com is forwarded.
//
// A certificate matches by both its certificate type (e.g.
// ssh-ed25519-cert-v01@openssh.com) and its underlying key's type, and
// its fingerprint is the underlying key's — so --allow-key-type can
// admit or deny certificates independently of static keys, and a key of
// an unknown or unparsable type matches no type list (denied when
// --allow-key-type is set). A key an allowlist denies is logged with a
// reason field naming the violated list(s) by flag name, so the trace
// says which flag to adjust; the error returned to the client is
// deliberately the same either way.
//
// A second gate is the session check. At handover, flue checks that the
// broker's session (learned from the handover message's credentials) is
// private: the only live process in it is the broker itself, which has
// not yet forked its wrapped command. Session membership is inherited by
// fork only, so such a session can later hold only the broker and its
// descendants — which is what an exec that created its own session
// looks like, with or without a tty (the broker is its leader, or the
// per-exec helper that led it has already exited). flue then verifies
// that each client shares the broker's session (SO_PEERCRED on the
// accepted connection, session id from /proc, and a SO_PEERPIDFD
// liveness check against the pid-reuse race) and refuses clients in any
// other session, failing closed when a session cannot be determined. An
// exec whose process shares a session with the container's other
// processes (e.g. the container's init session) is not private, and the
// check is off for it.
//
// The check's mode depends on how the broker was started (sessionCheckMode):
// an injected broker run with a tty (`flue enter` from a terminal) makes
// the check required — if it cannot be enabled (no broker credentials in
// the handover, the broker's session unreadable, or its session not
// private), flue aborts the session, failing closed rather than silently
// serving one without the second gate. Without injection — an enter with
// --no-inject-broker, or the flue exec passthrough, where the broker runs
// unmodified from the image — the broker is untrusted code that could
// hand over a listener from any session it controls, so the check is not
// used (a warning is logged). In the remaining case (an injected broker
// without a tty) the check is advisory: on when the broker's session is
// private, off otherwise; its state is logged at info at handover. On
// enter, --session-check=required|advisory|off overrides the choice (an
// explicit choice is logged at info; the default at handover).
//
// flue is a cobra CLI with four subcommands. The global flags (--sock,
// --allow-key, --allow-key-type, --log-level, --log-file) are persistent:
// place them before
// the subcommand. The subcommand's arguments follow the subcommand name;
// a --
// separates them from the target command so the target's own flags are
// passed through verbatim (cobra/pflag, unlike stdlib flag, keeps scanning
// for flags after positional arguments).
//
// Two flags also read environment variables as a fallback when the flag
// itself is not set on the command line: --log-level reads $FLUE_LOG_LEVEL,
// and enter --inject-broker reads $FLUE_INJECT_BROKER (a recognized value:
// truthy keeps the default on, falsy disables it). An explicitly set flag
// always wins; the env var only fills in when the flag is absent. The
// command enter runs by default (no command argument) is likewise
// overridable: $FLUE_ENTER_CMD (whitespace-split) replaces /bin/sh when
// set and non-blank; an explicit command always wins.
//
// exec — the passthrough form: run any command that receives the control
// socketpair (typically flue-broker, or a wrapper that preserves fd 3):
//
//	flue exec -- podman exec --preserve-fds 1 <container> flue-broker bash
//
// (wrapper mode: the broker sets SSH_AUTH_SOCK for its child). In
// standalone mode (no command after flue-broker), the caller must name the
// socket, so pin -sock to a fixed path and set SSH_AUTH_SOCK to it:
//
//	flue exec -- podman exec --preserve-fds 1 \
//	  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> \
//	  flue-broker -sock /tmp/flue.sock
//
// enter — the primary flow. It runs the podman exec for you, in wrapper
// mode (so the broker sets SSH_AUTH_SOCK for the wrapped command):
//
//	flue enter [container] [command [args…]]
//
// No container defaults to snugmo; no command defaults to
// $FLUE_ENTER_CMD (whitespace-split) when set and non-blank, else /bin/sh;
// a tty is allocated when stdin is a tty. A `--` separator takes
// precedence: the tokens after it are the command and the single
// positional argument before it — if any — names the container, so
// `flue enter -- bash` runs bash in the default container, not the
// default command in a container named bash. The broker is not taken from the
// image by default: it is injected in memory (a memfd the container execs
// via /proc/self/fd/4), so no writable filesystem or image-side broker is
// needed; --no-inject-broker (or --inject-broker=false) uses the image's.
// flue exits when the container command does, returning its exit code. The
// in-container socket path is unique per session (the broker generates it:
// XDG_RUNTIME_DIR, else /run/user/<uid>, else /run, else /tmp) and is
// exported to the wrapped command as SSH_AUTH_SOCK.
//
// extract-broker — writes the embedded broker back out:
//
//	flue extract-broker <filename>
//
// The result is a byte-identical copy of the built flue-broker (mode
// 0755). It needs no agent socket: the root PersistentPreRun resolves
// the agent socket only for the subcommands that proxy it (exec, enter).
//
// version — prints the build version (the same one-line string the root
// --version flag prints):
//
//	flue version
//
// The version is a package variable stamped at link time with
// -ldflags "-X main.version=<version>" (build.sh, the Containerfile, and
// the release workflow all do this); a plain `go build` reports "dev".
//
// flue also embeds a complete copy of the flue-broker built at the same
// time (see embed.go): go:embed reads cmd/flue/flue-broker.bin, which the
// build must produce by building flue-broker *first*; a missing file fails
// the compile, so a flue without its broker is never built silently. The
// embedded bytes are the built broker's, untransformed, so identical.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"
)

// sockArg and logLevel hold the raw values of the persistent --sock and
// --log-level flags (bound in init). Both default to the empty string, which
// means "not set on the command line": the root PersistentPreRun resolves
// each against its environment fallback ($SSH_AUTH_SOCK and $FLUE_LOG_LEVEL
// respectively) before falling back to a built-in default. sockPath is the
// resolved agent socket path (sockArg, or $SSH_AUTH_SOCK when the flag is
// empty), set by the root PersistentPreRun and shared by both the run*
// subcommands and the per-client dialAgent.
var (
	sockArg       string
	logLevel      string
	logFile       string
	allowKeys     = newAllowKeyFlag()
	allowKeyTypes = newAllowKeyTypeFlag()
	sockPath      string
	// enterInjectBroker holds the --inject-broker value for `enter`
	// (bound in init to both --inject-broker and its negative form
	// --no-inject-broker); it switches the in-container command from the
	// image's flue-broker to a broker exec'd from an in-memory memfd.
	// The enter command resolves the effective value (an explicit flag,
	// else $FLUE_INJECT_BROKER, else the default: on) in its Run hook
	// before runEnter reads it.
	enterInjectBroker bool
	// enterSessionCheck holds the raw --session-check value for `enter`
	// ("" = auto: the mode is chosen from how the broker was started,
	// see runEnter; required|advisory|off override it).
	enterSessionCheck string
)

// defaultEnterContainer is the container `flue enter` targets when none is
// given, and defaultEnterCmd the command run inside it when no explicit
// command is given; defaultEnterCmdEnv overrides that default command.
const (
	defaultEnterContainer = "snugmo"
	defaultEnterCmd       = "/bin/sh"
	defaultEnterCmdEnv    = "FLUE_ENTER_CMD"
)

// Limits on what a container-side client can make the host do: concurrent
// connections (each costs a goroutine and an upstream agent connection),
// the time to reach the host agent, and how long a connection may sit idle.
// Variables so tests can shorten them.
var (
	maxClients  = 64
	dialTimeout = 5 * time.Second
	idleTimeout = 10 * time.Minute
)

// handoverTimeout bounds how long flue waits for the broker's listener fd.
// It is a variable so tests can shorten it.
var handoverTimeout = 30 * time.Second

// procRoot is the root of the /proc tree the session check reads from.
// It is a variable so tests can point it at a temporary tree.
var procRoot = "/proc"

// log is the package-level slog logger. The default is rebuilt by the root
// PersistentPreRun from the --log-level and --log-file flags; the
// declaration-time default keeps tests (which never call main) working.
var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// version is the build version of this flue binary. It is a package
// variable so the build can stamp it at link time with
// -ldflags "-X main.version=<version>" (build.sh, the Containerfile, and
// the release workflow all do this); a plain `go build` leaves it at
// "dev". Shown by the root --version flag and the version subcommand.
var version = "dev"

// versionLine is the one-line version string both the root --version flag
// (cobra prints it) and the version subcommand print.
func versionLine() string { return fmt.Sprintf("flue version %s", version) }

// rootCmd is the top-level `flue` command. It carries the persistent
// (global) flags shared by the subcommands, the build version (cobra
// turns it into the --version flag), and a PersistentPreRun that sets up
// logging and resolves the agent socket path once, for whichever
// subcommand runs. Invoked with no subcommand it prints a usage error and
// exits 1 (the traditional bare-`flue` behavior).
var rootCmd = &cobra.Command{
	Use:     "flue",
	Short:   "Filtered SSH agent bridge from the host into a container",
	Version: version,
	Long: `flue proxies your host's SSH agent into a container with a key
allowlist. It creates a control socketpair, hands one end to the container
(as fd 3), receives the broker's listening socket, and proxies each client
connection to the real $SSH_AUTH_SOCK, forwarding only allowed keys.

The global flags (--sock, --allow-key, --allow-key-type, --log-level, --log-file) go before the
subcommand. The subcommand's arguments follow the subcommand name; a --
separates them from the target command, so the target command's own flags
are passed through verbatim.`,
	Run: func(cmd *cobra.Command, args []string) {
		// No subcommand given: the classic usage-error case.
		fmt.Fprintln(os.Stderr, `flue: missing subcommand (want "exec", "enter", "extract-broker", or "version")`)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, cmd.Root().UsageString())
		os.Exit(1)
	},
}

// needsAgentAnnotation marks the subcommands that proxy the host agent.
// The root PersistentPreRun resolves the agent socket only for annotated
// commands, so a subcommand that does not need an agent (extract-broker)
// runs without SSH_AUTH_SOCK being set.
const needsAgentAnnotation = "flue/needs-agent"

// execCmd is the passthrough form: it runs the given command (after --),
// which is expected to receive the control socketpair as fd 3.
var execCmd = &cobra.Command{
	Use:         "exec [command [args...]]",
	Short:       "Run a command that receives the control socketpair",
	Annotations: map[string]string{needsAgentAnnotation: "1"},
	Long: `exec runs any command that receives the control socketpair (fd 3)
itself — typically flue-broker, or a wrapper that preserves fd 3.

The command and its arguments follow the subcommand name; separate them from
flue's own flags with -- so the command's own flags are passed through:

    flue exec -- podman exec --preserve-fds 1 \
      -e SSH_AUTH_SOCK=/tmp/flue.sock <container> \
      flue-broker -sock /tmp/flue.sock

(the broker generates a unique per-session socket path unless -sock pins a
fixed one, so standalone mode must name the same path in both places).

flue blocks on the command exiting and returns its exit code.`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if code := runPassthrough(args, sockPath); code != 0 {
			os.Exit(code)
		}
	},
}

// enterCmd is sugar for the common case: run a command (default /bin/sh)
// in a container (default snugmo), wrapping it with flue-broker so the
// broker sets SSH_AUTH_SOCK for the child.
var enterCmd = &cobra.Command{
	Use:         "enter [container] [command [args...]]",
	Short:       "Run a command (default /bin/sh) in a container (default snugmo) with the agent",
	Annotations: map[string]string{needsAgentAnnotation: "1"},
	Long: `enter is the primary way to use flue: it runs a podman exec against
a container (default snugmo) with flue-broker wrapping the given command
(default $FLUE_ENTER_CMD when set and non-blank, else /bin/sh),
allocating a tty when stdin is a tty:

    flue enter                      # /bin/sh in the snugmo container
    flue enter <container>          # /bin/sh in the container
    flue enter <container> bash     # bash
    flue enter <container> -- bash -l # command with its own flags
    flue enter -- bash -l           # bash -l in the default container

A -- with no argument before it makes everything after it the command run
in the default container (not a container name); with a container before
it, the -- only separates the command's own flags from the rest.

The broker is not taken from the image by default: flue writes the embedded
flue-broker to an in-memory memfd (fd 4) and the container execs it via
/proc/self/fd/4, so a container with no writable filesystem — and no
flue-broker in its image — still gets the agent. Disable injection with
--no-inject-broker, --inject-broker=false, or FLUE_INJECT_BROKER=0 to use
the image's flue-broker instead. The control socketpair stays on fd 3
either way, and the broker exports its (unique per-session) socket path to
the wrapped command as SSH_AUTH_SOCK, so ssh, ssh-add, and git just work.
The default command is /bin/sh; $FLUE_ENTER_CMD (split on whitespace)
replaces it when no command is given on the command line:

    FLUE_ENTER_CMD=bash flue enter       # bash in snugmo

When the command exits, enter returns its exit code.`,
	Args: cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		// Resolve the effective --inject-broker: an explicit flag wins, else
		// $FLUE_INJECT_BROKER, else the default (on).
		enterInjectBroker = injectBrokerEffective(cmd)
		if code := runEnter(args, sockPath); code != 0 {
			os.Exit(code)
		}
	},
}

// versionCmd is the version subcommand: it prints this flue's build
// version — the same one-line string the root --version flag prints.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionLine())
	},
}

// init wires the persistent flags and subcommands onto rootCmd, and sets up
// the shared PersistentPreRun (log-level parse + agent-socket resolution).
func init() {
	rootCmd.PersistentFlags().StringVar(&sockArg, "sock", "",
		"agent socket path to proxy to (default $SSH_AUTH_SOCK)")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "",
		"log level: debug, info, warn, or error (default: $FLUE_LOG_LEVEL, else info)")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "",
		"append log output to this file instead of stderr (e.g. to keep refusal warnings off the user's terminal)")
	rootCmd.PersistentFlags().Var(allowKeys, "allow-key",
		"SHA256 fingerprint to allow (repeatable)")
	rootCmd.PersistentFlags().Var(allowKeyTypes, "allow-key-type",
		"key type to allow (repeatable); e.g. ssh-ed25519, ecdsa-sha2-nistp256, rsa")
	enterCmd.Flags().BoolVar(&enterInjectBroker, "inject-broker", true,
		"run the broker from an in-memory memfd (exec'd via /proc/self/fd/4) instead of the image's flue-broker; no writable filesystem needed (default: on; $FLUE_INJECT_BROKER applies when the flag is unset)")
	// pflag (unlike stdlib flag) does not auto-generate --no-<flag> forms
	// for bool flags, so the negative form of the default-on
	// --inject-broker is registered explicitly, bound to the same variable.
	// NoOptDefVal makes a bare --no-inject-broker (no =value) disable
	// injection instead of consuming the next argument.
	noInject := enterCmd.Flags().VarPF(newNegBoolValue(&enterInjectBroker), "no-inject-broker", "",
		"disable broker injection (use the image's flue-broker instead); the negative form of --inject-broker")
	noInject.NoOptDefVal = "true"
	enterCmd.Flags().StringVar(&enterSessionCheck, "session-check", "",
		"override the session check mode: required, advisory, or off (default: auto — required for an injected broker with a tty, off without injection, advisory otherwise)")
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		if lvl, err := parseLevel(resolveLogLevel()); err != nil {
			fatalf("flue: %v", err)
		} else {
			// --log-file moves the logs (including refusal warnings)
			// somewhere other than the user's tty; stderr otherwise.
			var out io.Writer = os.Stderr
			if logFile != "" {
				f, ferr := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if ferr != nil {
					fatalf("flue: open log file %s: %v", logFile, ferr)
				}
				out = f
			}
			log = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: lvl}))
		}
		if cmd.Annotations[needsAgentAnnotation] == "" {
			return // this subcommand does not proxy the agent socket
		}
		sockPath = sockArg
		if sockPath == "" {
			sockPath = os.Getenv("SSH_AUTH_SOCK")
			if sockPath == "" {
				fatalf("flue: no agent socket: set --sock or SSH_AUTH_SOCK")
			}
		}
	}
	rootCmd.AddCommand(execCmd, enterCmd, extractCmd, versionCmd)
}

// parseLevel maps a --log-level flag value to a slog.Level.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", s)
}

// resolveLogLevel returns the effective --log-level value: an explicit flag
// wins, else the FLUE_LOG_LEVEL environment variable, else the built-in
// default "info".
func resolveLogLevel() string {
	if logLevel != "" {
		return logLevel
	}
	if v := os.Getenv("FLUE_LOG_LEVEL"); v != "" {
		return v
	}
	return "info"
}

// envBool parses an environment-variable value as a boolean. It reports
// (value, recognized): recognized truthy (case-insensitive, trimmed):
// 1, t, true, y, yes, on; recognized falsy: 0, f, false, n, no, off.
// Unset, empty, or unrecognized values report recognized=false so the
// caller can fall back to its default.
func envBool(s string) (value bool, recognized bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, true
	case "0", "f", "false", "n", "no", "off":
		return false, true
	}
	return false, false
}

// injectBrokerEffective resolves the effective --inject-broker value for
// `enter`: an explicitly set --inject-broker or --no-inject-broker flag
// wins (each sets the shared variable directly), else a recognized
// FLUE_INJECT_BROKER environment variable, else the default: on.
func injectBrokerEffective(cmd *cobra.Command) bool {
	for _, name := range []string{"inject-broker", "no-inject-broker"} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return enterInjectBroker
		}
	}
	if v, ok := envBool(os.Getenv("FLUE_INJECT_BROKER")); ok {
		return v
	}
	return true
}

// negBoolValue is a pflag Value that sets its bound variable to the
// negation of its parsed value — the pflag equivalent of the stdlib flag
// package's `no-` prefix (pflag does not auto-generate --no-<flag> forms
// for bool flags). newNegBoolValue registers the variable at its default
// (true), matching the flag it negates.
type negBoolValue bool

func newNegBoolValue(p *bool) *negBoolValue {
	*p = true
	return (*negBoolValue)(p)
}

// n aliases the bound bool (newNegBoolValue converted *bool to
// *negBoolValue), so writing *n writes the shared flag variable.
func (n *negBoolValue) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*n = negBoolValue(!v)
	return nil
}

func (n *negBoolValue) Type() string   { return "bool" }
func (n *negBoolValue) String() string { return strconv.FormatBool(bool(*n)) }

// fatalf prints the formatted message to stderr and exits 1, keeping the
// traditional CLI fatal behavior (a plain-text line, as log.Fatalf used
// to produce) rather than a slog record — usage and flag errors in
// particular must read as ordinary text. The format string is
// printf-substituted; note that slog does not interpret format verbs (it
// treats the args as key/value pairs). slog is used for the non-fatal,
// leveled operational logging.
func fatalf(format string, args ...any) {
	fmt.Fprintln(os.Stderr, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func main() {
	// cobra handles flag parsing, subcommand dispatch, and --help. An error
	// from here is a flag or argument-validation failure (unknown flag,
	// unknown subcommand, too few args); cobra has already printed it, so we
	// just exit non-zero. Fatal setup errors exit earlier, via fatalf in the
	// PersistentPreRun.
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// runEnter is the `flue enter` subcommand: it runs `podman exec` against
// a container (default snugmo) with flue-broker wrapping the given command
// (the default command, see enterDefaultCmd), allocating a tty when stdin
// is a tty. The control socketpair is preserved through podman exec as
// fd 3 (--preserve-fds 1, or 2 when the broker memfd is also passed) and
// the agent is proxied exactly as in passthrough mode.
func runEnter(args []string, sockPath string) int {
	container, cmd, err := enterTarget(args)
	if err != nil {
		fatalf("flue: %v", err)
	}

	tty := stdinIsTTY()

	// The session check's mode: an injected broker with a tty is the
	// interactive session the check exists to protect, so the check is
	// required (a session without it is aborted); without injection the
	// broker is untrusted code and the check is not used; otherwise
	// (an injected broker without a tty) it is advisory. Resolved before
	// the memfd and exec below so a bad --session-check value fails fast.
	var checkMode sessionCheckMode
	switch {
	case tty && enterInjectBroker:
		checkMode = sessionCheckRequired
	case !enterInjectBroker:
		checkMode = sessionCheckOff
	default:
		checkMode = sessionCheckAdvisory
	}
	// --session-check overrides the choice (the escape hatch for
	// runtimes whose execs share a session — e.g. the container's init
	// session — so an interactive enter's required check always aborts).
	// The default choice is logged at handover; an explicit one here.
	if enterSessionCheck != "" {
		mode, err := parseSessionCheckMode(enterSessionCheck)
		if err != nil {
			fatalf("flue: enter: %v", err)
		}
		checkMode = mode
		log.Info("flue: session check mode (explicit)", "mode", enterSessionCheck)
	}

	// Broker injection (default on): carry the embedded broker in a memfd
	// (fd 4) instead of relying on the image's flue-broker, so no writable
	// filesystem is needed.
	var preserved []*os.File
	if enterInjectBroker {
		mf, err := injectBrokerMemfd()
		if err != nil {
			fatalf("flue: enter: %v", err)
		}
		defer closeFile(mf)
		preserved = []*os.File{mf}
	}

	execArgs := enterExecArgs(container, cmd, tty, enterInjectBroker)

	podman, err := exec.LookPath("podman")
	if err != nil {
		fatalf("flue: enter: find podman: %v", err)
	}

	return runChildWithAgent(podman, execArgs, sockPath, preserved, checkMode, enterSessionCheck != "")
}

// parseSessionCheckMode maps a --session-check value to a
// sessionCheckMode.
func parseSessionCheckMode(s string) (sessionCheckMode, error) {
	switch s {
	case "required":
		return sessionCheckRequired, nil
	case "advisory":
		return sessionCheckAdvisory, nil
	case "off":
		return sessionCheckOff, nil
	}
	return 0, fmt.Errorf("--session-check must be required, advisory, or off (got %q)", s)
}

// enterTarget splits the `flue enter` arguments into the container name
// (default snugmo) and the command to run inside it (the default command,
// see enterDefaultCmd). A `--` separator (recovered from the raw process
// arguments, see enterDashDash) takes precedence over that split: the
// tokens after it are the command, and the single positional argument
// before it — if any — names the container, so `flue enter -- bash` runs
// bash in the default container instead of the default command in a
// container named bash. A separator with nothing after it is a no-op.
// More than one positional argument before the separator is a usage
// error.
func enterTarget(args []string) (container string, cmd []string, err error) {
	if dashCmd, preCount, present := enterDashDash(args); present && len(dashCmd) > 0 {
		switch preCount {
		case 0:
			return defaultEnterContainer, dashCmd, nil
		case 1:
			return args[0], dashCmd, nil
		default:
			return "", nil, fmt.Errorf("enter: at most one argument (a container name) may precede -- (got %d)", preCount)
		}
	}
	if len(args) == 0 {
		return defaultEnterContainer, enterDefaultCmd(), nil
	}
	container = args[0]
	if len(args) > 1 {
		return container, args[1:], nil
	}
	return container, enterDefaultCmd(), nil
}

// enterDashDash recovers the `--` separator from the raw process
// arguments for the enter subcommand: cobra (via pflag) consumes the
// first one before Run, so it is not in the positional arguments
// enterTarget receives, but its presence changes how they split — the
// tokens after the last separator are the target command, and the
// positional arguments before it (at most one) name the container. It
// returns the tokens after the last separator, the number of positional
// arguments before the first separator, and whether a separator appears
// after the subcommand (found as the first `enter` word in os.Args, which
// no global flag value can precede it as a subcommand).
//
// The count is derived arithmetically: pflag consumes only the first
// separator and appends the remaining tokens verbatim (any later `--`
// included), so the positional arguments cobra hands to Run are exactly
// the pre-separator positional arguments plus everything after the first
// separator.
func enterDashDash(args []string) (cmd []string, preCount int, present bool) {
	i := 0
	for i < len(os.Args) && os.Args[i] != "enter" {
		i++
	}
	if i == len(os.Args) {
		return nil, 0, false
	}
	rest := os.Args[i+1:]
	first, last := -1, -1
	for j, s := range rest {
		if s == "--" {
			if first == -1 {
				first = j
			}
			last = j
		}
	}
	if last == -1 {
		return nil, 0, false
	}
	return rest[last+1:], len(args) - len(rest[first+1:]), true
}

// enterDefaultCmd resolves the command `flue enter` runs when no explicit
// command is given: $FLUE_ENTER_CMD (split on whitespace) when set and
// non-blank, else /bin/sh.
func enterDefaultCmd() []string {
	if v, ok := os.LookupEnv(defaultEnterCmdEnv); ok {
		if fields := strings.Fields(v); len(fields) > 0 {
			return fields
		}
	}
	return []string{defaultEnterCmd}
}

// enterExecArgs builds the argument list for the `podman exec` that runs
// flue-broker (wrapping cmd) inside the container. A tty is requested
// only when the caller's stdin is a tty.
func enterExecArgs(container string, cmd []string, tty, injectBroker bool) []string {
	out := []string{"exec"}
	if tty {
		out = append(out, "-ti")
	} else {
		out = append(out, "-i")
	}
	if injectBroker {
		// Two preserved fds: fd 3 = the control socketpair, fd 4 = the broker
		// memfd. The container execs the broker from /proc/self/fd/4, so the
		// broker needs neither a filesystem path nor a writable filesystem.
		out = append(out,
			"--preserve-fds", "2",
			container,
			"/proc/self/fd/4",
		)
	} else {
		out = append(out,
			"--preserve-fds", "1",
			container,
			"flue-broker",
		)
	}
	return append(out, cmd...)
}

// stdinIsTTY reports whether stdin is a terminal, using the TCGETS ioctl
// (which fails for pipes, regular files, and sockets).
func stdinIsTTY() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

// runPassthrough is the classic mode: the caller passes a command (after
// `--`) that is expected to receive the control socketpair (flue-broker,
// or a wrapper that preserves fd 3). The broker runs unmodified from the
// image, so it is untrusted code (like --no-inject-broker) and the
// session check is off here: a warning says why.
func runPassthrough(args []string, sockPath string) int {
	cmdPath := args[0]
	if !strings.Contains(cmdPath, "/") {
		if resolved, err := exec.LookPath(cmdPath); err == nil {
			cmdPath = resolved
			args[0] = resolved
		}
	}

	return runChildWithAgent(cmdPath, args[1:], sockPath, nil, sessionCheckOff, false)
}

// runChildWithAgent starts a child (program plus args), hands one end of
// a SOCK_SEQPACKET socketpair to it as fd 3, receives the broker's
// listening fd over the other end, and proxies each accepted client
// connection to the host agent with filtering applied. It blocks until
// the child exits and returns the child's exit code (0 on success). The
// lifetime model — the control connection stays open until the broker is
// gone, and the block is on the child, not the channel — is in the file
// header. checkExplicit marks a mode the user selected with
// --session-check (rather than one chosen from how the broker was
// started): the handover log then attributes the check's state to the
// explicit choice, not to the auto rule.
func runChildWithAgent(cmdPath string, cmdArgs []string, sockPath string, preserved []*os.File, checkMode sessionCheckMode, checkExplicit bool) int {
	// The container takes end B (fd 3); the host keeps end A.
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fatalf("flue: socketpair: %v", err)
	}
	a, b := pair[0], pair[1]

	// Enable SO_PASSCRED on the host end: the kernel then attaches the
	// sender's credentials (SCM_CREDENTIALS — the broker's pid in flue's
	// pid namespace) to the handover message, so
	// receiveListener can learn the broker's session id for the client
	// session check. If this fails, the broker's session stays unknown
	// (0): in advisory mode the session check is off (logged at info),
	// in required mode the session is aborted (fail closed).
	if err := unix.SetsockoptInt(a, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		log.Warn("flue: enable SO_PASSCRED", "err", err)
	}

	// Pass end B as an extra file in the child; it will be fd 3
	// (first extra file after stdin/stdout/stderr).
	childFd := os.NewFile(uintptr(b), "flue-control-child")
	defer closeFile(childFd)

	cmd := exec.Command(cmdPath, cmdArgs...)
	// The control socketpair is always the first preserved fd (fd 3). Any
	// additional preserved files follow in order as fd 4, 5, ... — for
	// --inject-broker this is the broker memfd (fd 4).
	cmd.ExtraFiles = append([]*os.File{childFd}, preserved...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fatalf("flue: start %s: %v", cmdPath, err)
	}

	// Close the host copy of end B; the child now owns it. This goes
	// through childFd (which owns the fd) rather than a raw unix.Close, so
	// the deferred close above is a harmless no-op instead of a second
	// close of a possibly reused fd number.
	closeFile(childFd)

	// The host's control socket is end A. net.FileConn dups the fd, so the
	// os.File is closed right away; left open it would keep the original
	// fd (and thus the socket's peer state) alive until a GC finalizer.
	af := os.NewFile(uintptr(a), "flue-control-host")
	ctrl, err := net.FileConn(af)
	closeFile(af)
	if err != nil {
		fatalf("flue: control fd: %v", err)
	}

	pol := &policy{
		allow:                allowKeys.toSet(),
		allowTypes:           allowKeyTypes.toSet(),
		sock:                 sockPath,
		sessionCheckMode:     checkMode,
		sessionCheckExplicit: checkExplicit,
		refusals:             &refusalLimiter{},
	}

	log.Info("flue: agent", "sock", sockPath, "filter", allowKeys.describe(), "filter-types", allowKeyTypes.describe(), "exec", append([]string{cmdPath}, cmdArgs...))

	clients := clientSet{max: maxClients}
	listener, acceptDone, checkErr := receiveListener(ctrl, pol, &clients)

	// The session check was required but cannot be enabled: fail closed.
	// Close the control connection now — before the wait below, which
	// would otherwise deadlock: the broker keeps its wrapped command
	// alive until the host goes away — so the broker kills the command
	// and exits, and this session is never served without the check.
	if checkErr != nil {
		log.Error("flue: session aborted", "err", checkErr)
		closeQuiet(ctrl)
	}

	err = cmd.Wait()

	// The broker is gone: close the one-shot control connection (kept open
	// for the child's lifetime so the broker never sees a premature "host
	// gone"; already closed in the abort case above, where this is a
	// no-op), stop the accept loop, and close the remaining client
	// connections. The command is gone, so nothing legitimate is left to
	// serve, and a lingering container process holding a connection open
	// would otherwise block the wait below forever.
	closeQuiet(ctrl)
	if listener != nil {
		closeListener(listener)
	}
	<-acceptDone
	clients.closeAll()
	clients.Wait()
	// Report any suppressed refusal warnings: the last word on how many
	// clients the session check refused.
	pol.refusals.flush()

	if checkErr != nil {
		return 1
	}
	if err == nil {
		return 0
	}
	code := 1
	if ee, ok := err.(*exec.ExitError); ok {
		code = exitStatus(ee)
	}
	log.Warn("flue: command exited", "err", err)
	return code
}

// exitStatus maps an ExitError to a shell-style exit code: the exit status,
// or 128+signal when the process was killed by a signal (ExitCode alone
// reports -1 for that, which os.Exit would turn into 255).
func exitStatus(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// --- allow-key repeatable flag ---

type allowKeyFlag struct {
	vals []string
}

func newAllowKeyFlag() *allowKeyFlag { return &allowKeyFlag{} }

func (a *allowKeyFlag) String() string { return strings.Join(a.vals, ",") }

// Type satisfies pflag.Value, which extends stdlib flag.Value with a type
// name. cobra registers --allow-key as a pflag flag, so this is required.
func (a *allowKeyFlag) Type() string { return "string" }

func (a *allowKeyFlag) Set(s string) error {
	a.vals = append(a.vals, s)
	return nil
}

// toSet returns a map of fingerprint → struct{} for O(1) lookups.
func (a *allowKeyFlag) toSet() map[string]struct{} {
	m := make(map[string]struct{}, len(a.vals))
	for _, v := range a.vals {
		m[v] = struct{}{}
	}
	return m
}

// describe returns a human-readable summary for logging.
func (a *allowKeyFlag) describe() string {
	if len(a.vals) == 0 {
		return "all keys"
	}
	return strings.Join(a.vals, ", ")
}

// --- allow-key-type repeatable flag ---

// allowKeyTypeFlag collects the --allow-key-type values (SSH key type wire
// names, e.g. ssh-ed25519, ecdsa-sha2-nistp256, rsa). Like allowKeyFlag it
// implements pflag.Value so the flag is repeatable.
type allowKeyTypeFlag struct {
	vals []string
}

func newAllowKeyTypeFlag() *allowKeyTypeFlag { return &allowKeyTypeFlag{} }

func (a *allowKeyTypeFlag) String() string { return strings.Join(a.vals, ",") }

// Type satisfies pflag.Value, which extends stdlib flag.Value with a type
// name. cobra registers --allow-key-type as a pflag flag, so this is
// required.
func (a *allowKeyTypeFlag) Type() string { return "string" }

func (a *allowKeyTypeFlag) Set(s string) error {
	a.vals = append(a.vals, s)
	return nil
}

// toSet returns a map of key type → struct{} for O(1) lookups.
func (a *allowKeyTypeFlag) toSet() map[string]struct{} {
	m := make(map[string]struct{}, len(a.vals))
	for _, v := range a.vals {
		m[v] = struct{}{}
	}
	return m
}

// describe returns a human-readable summary for logging.
func (a *allowKeyTypeFlag) describe() string {
	if len(a.vals) == 0 {
		return "all types"
	}
	return strings.Join(a.vals, ", ")
}

// --- agent filter ---

// policy is the session-wide agent policy: the --allow-key set (empty
// means allow all), the --allow-key-type set (empty means allow all
// types), the path of the host agent socket that each client
// dials, and the broker's pid and session id (0 = unknown) learned from
// the handover message's credentials. It is built once in
// runChildWithAgent and shared across all per-client agentFilter values;
// the broker fields are set once more, by receiveListener.
type policy struct {
	allow                map[string]struct{} // empty = allow all
	allowTypes           map[string]struct{} // empty = allow all types
	sock                 string              // host agent socket to dial per client
	sessionCheckMode     sessionCheckMode    // how the session check is enforced (see its type)
	brokerPID            int                 // broker's pid in flue's pid ns (0 = unknown)
	brokerSession        int                 // broker's session id (0 = unknown)
	sessionPrivate       bool                // broker's session had no other live member at handover
	sessionPrivateKnown  bool                // the privacy scan ran (false = could not be determined)
	sessionCheckExplicit bool                // the mode came from --session-check, not the auto rule
	refusals             *refusalLimiter     // rate-limits the refusal warnings (lograte.go)
}

// agentFilter wraps one upstream ssh agent client (one per client
// connection) and implements agent.ExtendedAgent with the session's key
// allowlist policy.
type agentFilter struct {
	upstream agent.ExtendedAgent
	pol      *policy
}

// isAllowed checks whether a key's fingerprint is in the allowlist.
func (f *agentFilter) isAllowed(fp string) bool {
	if len(f.pol.allow) == 0 {
		return true
	}
	_, ok := f.pol.allow[fp]
	return ok
}

// logf traces one agent operation at debug level (enable with
// --log-level=debug; the former -verbose flag). For ops about a specific
// key, ktype is the key type (e.g. ssh-ed25519) and detail the key
// fingerprint, logged as the type and fingerprint fields; for the rest,
// ktype is empty and detail a summary (or empty). When an allowlist
// denies the key, reasons names the violated rules by their flag names
// (allow-key, allow-key-type) and is logged as the reason field, so an
// operator can tell which flag to adjust.
func (f *agentFilter) logf(op, ktype, detail string, allowed bool, reasons []string) {
	args := []any{"op", op}
	if ktype != "" {
		args = append(args, "type", ktype, "fingerprint", detail)
	} else {
		args = append(args, "detail", detail)
	}
	args = append(args, "allowed", allowed)
	if r := strings.Join(reasons, ","); r != "" {
		args = append(args, "reason", r)
	}
	log.Debug("flue: agent op", args...)
}

// keyVerdict checks a key against both allowlist dimensions and reports
// whether it is allowed and, when not, the names of the violated rules
// (the --allow-key and --allow-key-type flag names) for the trace's
// reason field.
func (f *agentFilter) keyVerdict(types []string, fp string) (bool, []string) {
	var reasons []string
	if !f.isAllowed(fp) {
		reasons = append(reasons, "allow-key")
	}
	if !f.typeAllowed(types) {
		reasons = append(reasons, "allow-key-type")
	}
	return len(reasons) == 0, reasons
}

// keyFingerprint computes the SHA256: fingerprint of an ssh.PublicKey the
// way ssh-add -l prints it: for a certificate that is the fingerprint of
// the underlying key (cert.Key), not of the certificate blob, so
// --allow-key values copied from ssh-add -l match certified keys.
func keyFingerprint(key ssh.PublicKey) string {
	if cert, ok := key.(*ssh.Certificate); ok {
		return ssh.FingerprintSHA256(cert.Key)
	}
	return ssh.FingerprintSHA256(key)
}

// keyFingerprintFromBlob computes the SHA256: fingerprint of a raw key
// blob the same way: if the blob is a certificate, the fingerprint of the
// underlying key; otherwise the SHA256 of the blob itself — identical to
// ssh.FingerprintSHA256 for a plain key, since its wire serialization is
// exactly the blob. An unparsable blob falls back to the raw hash, which
// matches no --allow-key value (and nothing else either).
func keyFingerprintFromBlob(blob []byte) string {
	key, err := ssh.ParsePublicKey(blob)
	if err == nil {
		return keyFingerprint(key)
	}
	h := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// keyTypeFromBlob returns the wire type of a raw key blob for logging.
// For a certificate that is the certificate's own type (e.g.
// ssh-ed25519-cert-v01@openssh.com), so certificate use is visible in the
// trace (the fingerprint, in contrast, is normalized to the underlying
// key — see keyFingerprintFromBlob). An unparsable blob yields "unknown".
func keyTypeFromBlob(blob []byte) string {
	key, err := ssh.ParsePublicKey(blob)
	if err != nil {
		return "unknown"
	}
	return key.Type()
}

// keyTypes returns the type names a key matches against --allow-key-type:
// a plain key has exactly its own wire type, while a certificate has both
// its certificate type (e.g. ssh-ed25519-cert-v01@openssh.com) and its
// underlying key's type (ssh-ed25519) — so a certificate can be allowed
// or excluded through either name, which is what lets a cert-only
// allowlist deny static (uncertified) keys.
func keyTypes(key ssh.PublicKey) []string {
	if cert, ok := key.(*ssh.Certificate); ok {
		return []string{cert.Type(), cert.Key.Type()}
	}
	return []string{key.Type()}
}

// keyTypesFromBlob is the blob variant of keyTypes: nil for an unparsable
// blob, which matches no --allow-key-type value (the filter fails closed
// when a type list is set).
func keyTypesFromBlob(blob []byte) []string {
	key, err := ssh.ParsePublicKey(blob)
	if err != nil {
		return nil
	}
	return keyTypes(key)
}

// typeAllowed reports whether any of the key's type names is in the
// --allow-key-type set; an empty set allows all types.
func (f *agentFilter) typeAllowed(types []string) bool {
	if len(f.pol.allowTypes) == 0 {
		return true
	}
	for _, t := range types {
		if _, ok := f.pol.allowTypes[t]; ok {
			return true
		}
	}
	return false
}

// List returns keys, filtered by the allowlists if either is set.
func (f *agentFilter) List() ([]*agent.Key, error) {
	keys, err := f.upstream.List()
	if err != nil {
		return nil, err
	}
	if len(f.pol.allow) == 0 && len(f.pol.allowTypes) == 0 {
		f.logf("list", "", fmt.Sprintf("%d keys", len(keys)), true, nil)
		return keys, nil
	}
	var out []*agent.Key
	for _, k := range keys {
		fp := keyFingerprintFromBlob(k.Blob)
		allowed, reasons := f.keyVerdict(keyTypesFromBlob(k.Blob), fp)
		if allowed {
			out = append(out, k)
		} else {
			f.logf("list", keyTypeFromBlob(k.Blob), fp, false, reasons)
		}
	}
	f.logf("list", "", fmt.Sprintf("%d of %d keys allowed", len(out), len(keys)), true, nil)
	return out, nil
}

// Sign forwards the signing request only for allowed keys.
func (f *agentFilter) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	fp := keyFingerprint(key)
	allowed, reasons := f.keyVerdict(keyTypes(key), fp)
	if !allowed {
		f.logf("sign", key.Type(), fp, false, reasons)
		return nil, fmt.Errorf("signing denied for key %s", fp)
	}
	f.logf("sign", key.Type(), fp, true, nil)
	return f.upstream.Sign(key, data)
}

// SignWithFlags is like Sign but with flags (needed for RSA rsa-sha2-256/512).
func (f *agentFilter) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	fp := keyFingerprint(key)
	allowed, reasons := f.keyVerdict(keyTypes(key), fp)
	if !allowed {
		f.logf("sign(flags)", key.Type(), fp, false, reasons)
		return nil, fmt.Errorf("signing denied for key %s", fp)
	}
	f.logf("sign(flags)", key.Type(), fp, true, nil)
	return f.upstream.SignWithFlags(key, data, flags)
}

// Add is always denied.
func (f *agentFilter) Add(k agent.AddedKey) error {
	f.logf("add", "", "", false, nil)
	return fmt.Errorf("adding keys is not allowed through flue")
}

// Remove is always denied.
func (f *agentFilter) Remove(key ssh.PublicKey) error {
	f.logf("remove", key.Type(), keyFingerprint(key), false, nil)
	return fmt.Errorf("removing keys is not allowed through flue")
}

// RemoveAll is always denied.
func (f *agentFilter) RemoveAll() error {
	f.logf("remove-all", "", "", false, nil)
	return fmt.Errorf("removing all keys is not allowed through flue")
}

// Lock is always denied.
func (f *agentFilter) Lock(passphrase []byte) error {
	f.logf("lock", "", "", false, nil)
	return fmt.Errorf("locking the agent is not allowed through flue")
}

// Unlock is always denied.
func (f *agentFilter) Unlock(passphrase []byte) error {
	f.logf("unlock", "", "", false, nil)
	return fmt.Errorf("unlocking the agent is not allowed through flue")
}

// Signers returns signers for the given key blob.
func (f *agentFilter) Signers() ([]ssh.Signer, error) {
	f.logf("signers", "", "", false, nil)
	return nil, fmt.Errorf("signers request is not allowed through flue")
}

// Extension forwards only session-bind@openssh.com; everything else is
// unsupported.
func (f *agentFilter) Extension(extensionType string, req []byte) (resp []byte, err error) {
	if extensionType == "session-bind@openssh.com" {
		return f.upstream.Extension(extensionType, req)
	}
	return nil, agent.ErrExtensionUnsupported
}

// --- control channel ---

// closeQuiet closes a connection, logging the error unless it is a
// benign "use of closed network connection".
func closeQuiet(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Debug("flue: close", "err", err)
	}
}

// closeListener closes a listener, logging the error unless it is a
// benign "use of closed network connection".
func closeListener(l net.Listener) {
	if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Debug("flue: close listener", "err", err)
	}
}

// closeFD closes a raw file descriptor, logging the error unless it is
// already closed.
func closeFD(fd int) {
	if err := unix.Close(fd); err != nil && !errors.Is(err, unix.EBADF) {
		log.Debug("flue: close fd", "fd", fd, "err", err)
	}
}

// closeFile closes an os.File, logging the error unless it is already
// closed.
func closeFile(f *os.File) {
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		log.Debug("flue: close file", "err", err)
	}
}

// receiveListener reads the single control message (the broker's listening
// fd, plus the broker's credentials) from the control channel, wraps the fd
// in a *net.UnixListener, and starts a background accept loop that proxies
// each client connection to the agent. The sender's credentials
// (SCM_CREDENTIALS) ride the same message; receiveListener turns the
// broker's pid into its session id and stores it on the policy
// (0 = unknown: no credentials, or the broker already gone). The control
// message is one-shot, but the control connection is NOT closed here: the
// caller keeps it open until the broker is gone, so the broker (which reads
// its own end) does not observe a premature "host gone". The caller closes
// ctrl.
// The returned error is non-nil only in sessionCheckRequired mode when
// the check cannot be enabled; the listener is then nil and the session
// must be aborted (the caller closes the control connection, which makes
// the broker kill its wrapped command).
func receiveListener(ctrl net.Conn, pol *policy, clients *clientSet) (net.Listener, <-chan struct{}, error) {
	buf := make([]byte, 4096)
	oob := make([]byte, 4096)

	uc := ctrl.(*net.UnixConn)
	// Bound the wait: a broker that starts but never hands over a listener
	// would otherwise leave flue blocked here silently. (A broker that
	// exits closes the peer, which wakes the read with EOF on its own.)
	_ = uc.SetReadDeadline(time.Now().Add(handoverTimeout))
	_, oobn, flags, _, err := uc.ReadMsgUnix(buf, oob)
	_ = uc.SetReadDeadline(time.Time{})
	if err != nil {
		switch {
		case err == io.EOF || err == unix.ECONNRESET:
			log.Warn("flue: control channel closed before listener received")
		case errors.Is(err, os.ErrDeadlineExceeded):
			log.Warn("flue: no listener received from the broker", "timeout", handoverTimeout)
		default:
			log.Warn("flue: control read", "err", err)
		}
		return nil, closedChannel(), nil
	}
	// The sender is the container side. Whatever fds arrived are ours to
	// close, including when the message is malformed or truncated.
	fds, perr := receivedFDs(oob[:oobn])
	if perr != nil {
		log.Warn("flue: parse control message", "err", perr)
	}
	// The broker's credentials (SO_PASSCRED on the host end, see
	// runChildWithAgent) ride the same message: a Ucred carrying the
	// broker's pid in flue's pid namespace, from which we read its
	// session id for the client session check. Unknown (0) when absent
	// or gone.
	if cred, cerr := receivedUcreds(oob[:oobn]); cerr == nil && cred.Pid > 0 {
		pol.brokerPID = int(cred.Pid)
		if pol.sessionCheckMode == sessionCheckOff {
			// The broker is untrusted code: the session it reports is
			// not a trustworthy reference (a modified broker could
			// hand over a listener from any session it controls), so
			// the privacy scan is skipped and the check stays off.
			log.Debug("flue: broker session", "pid", cred.Pid)
		} else if sess, serr := sessionIDOfPID(int(cred.Pid)); serr != nil {
			log.Warn("flue: broker session", "pid", cred.Pid, "err", serr)
		} else {
			pol.brokerSession = sess
			log.Info("flue: broker session", "pid", cred.Pid, "session", sess)
			if private, perr := sessionIsPrivate(pol.brokerPID, sess); perr != nil {
				log.Warn("flue: broker session", "pid", cred.Pid, "session", sess, "privacy-scan", perr)
			} else {
				pol.sessionPrivate = private
				pol.sessionPrivateKnown = true
			}
		}
	} else if cerr != nil {
		log.Warn("flue: no broker credentials in handover message", "err", cerr)
	}
	// The state of the client session check for this session, decided
	// once here so the user can see whether the second gate applies. In
	// required mode (injected broker with a tty) a check that cannot be
	// enabled is a session failure: the caller aborts (fail closed).
	switch {
	case pol.sessionCheckMode == sessionCheckOff:
		if pol.sessionCheckExplicit {
			log.Warn("flue: session check off: explicitly disabled with --session-check=off")
		} else {
			log.Warn("flue: session check off: the broker was not injected, so it is untrusted code: a modified broker could hand over a listener from any session it controls and make the check a no-op")
		}
	case sessionCheckActive(pol):
		log.Info("flue: session check on", "session", pol.brokerSession)
	case pol.sessionCheckMode == sessionCheckRequired:
		// The received fds (if any) would be closed later only on the
		// normal path; close them here since the session ends now.
		for _, fd := range fds {
			closeFD(fd)
		}
		return nil, closedChannel(),
			fmt.Errorf("session check is required but could not be enabled: %s", sessionCheckOffReason(pol))
	default:
		log.Info("flue: session check off", "reason", sessionCheckOffReason(pol))
	}
	if flags&unix.MSG_CTRUNC != 0 {
		log.Warn("flue: control message truncated; discarding")
		perr = errors.New("truncated")
	}
	if perr != nil {
		for _, fd := range fds {
			closeFD(fd)
		}
		return nil, closedChannel(), nil
	}
	// Expect exactly one listener fd; use the first, close any extras.
	listenFd := -1
	for _, r := range fds {
		if listenFd < 0 {
			listenFd = r
		} else {
			closeFD(r)
		}
	}
	if listenFd < 0 {
		log.Warn("flue: no listener fd received")
		return nil, closedChannel(), nil
	}

	// The received fd must be what the broker hands over: a listening Unix
	// stream socket. FileListener alone accepts any listening socket (a
	// compromised container could hand over a TCP listener), so validate
	// with getsockopt on the raw fd — the same pattern the broker uses for
	// its control fd (checkControlFD). On any mismatch: log, close, reject.
	domain, eDomain := unix.GetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_DOMAIN)
	typ, eType := unix.GetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_TYPE)
	acceptConn, eAccept := unix.GetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if eDomain != nil || eType != nil || eAccept != nil ||
		domain != unix.AF_UNIX || typ != unix.SOCK_STREAM || acceptConn != 1 {
		log.Warn("flue: received fd is not a listening Unix stream socket",
			"domain", domain, "type", typ, "acceptconn", acceptConn,
			"err", errors.Join(eDomain, eType, eAccept))
		closeFD(listenFd)
		return nil, closedChannel(), nil
	}

	// Wrap the received listener fd in a *net.UnixListener so Accept works.
	// FileListener dups the fd, so close the os.File right away: otherwise
	// the original stays open and the socket keeps listening even after the
	// caller closes the returned listener. The type assertion rejects
	// anything that is not a Unix listener, even though the getsockopt
	// check above should already have excluded it.
	lf := os.NewFile(uintptr(listenFd), "flue-listener")
	fileListener, err := net.FileListener(lf)
	closeFile(lf)
	if err != nil {
		log.Warn("flue: listener fd", "err", err)
		return nil, closedChannel(), nil
	}
	listener, ok := fileListener.(*net.UnixListener)
	if !ok {
		closeListener(fileListener)
		log.Warn("flue: listener fd did not wrap as a Unix listener")
		return nil, closedChannel(), nil
	}
	log.Debug("flue: accepting on handed-over listener")

	// Accept client connections in a background loop; it ends when the
	// listener is closed by the caller.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		delay := time.Duration(0)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					log.Debug("flue: accept loop done", "err", err)
					return
				}
				// Transient failures (EMFILE, ECONNABORTED, ...) must not
				// end the loop: the command keeps running and would
				// silently lose its agent. Back off and retry.
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else if delay *= 2; delay > time.Second {
					delay = time.Second
				}
				log.Warn("flue: accept", "err", err, "retry-in", delay)
				time.Sleep(delay)
				continue
			}
			delay = 0
			if !checkClientSession(conn, pol) {
				closeQuiet(conn)
				continue
			}
			if !clients.add(conn) {
				// Rate-limited like the session-check refusals
				// (lograte.go): a container process can connect in a
				// loop to flood the user's terminal.
				pol.refusals.reject(slog.LevelWarn, "flue: rejecting client connection (too many clients or shutting down)")
				closeQuiet(conn)
				continue
			}
			go func(c net.Conn) {
				defer clients.done(c)
				handleClient(c, pol)
			}(conn)
		}
	}()

	return listener, acceptDone, nil
}

// clientSet tracks the live client connections so shutdown can close them
// and wait for their handlers. The embedded WaitGroup counts handlers.
type clientSet struct {
	sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	max    int // live-connection cap; 0 means unlimited
	closed bool
}

// add registers c and counts its handler; it reports false (and registers
// nothing) once closeAll has run or when the live-connection cap is hit.
func (s *clientSet) add(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || (s.max > 0 && len(s.conns) >= s.max) {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[c] = struct{}{}
	s.Add(1)
	return true
}

// done unregisters c and marks its handler finished.
func (s *clientSet) done(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.Done()
}

// closeAll closes every live client connection, which unblocks their
// handlers, and refuses new ones.
func (s *clientSet) closeAll() {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		closeQuiet(c)
	}
	s.mu.Unlock()
}

// receivedFDs extracts every SCM_RIGHTS fd from a control-message buffer.
// It returns the fds found even when it also returns an error, so the
// caller can close them rather than leak them.
func receivedFDs(oob []byte) ([]int, error) {
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	var firstErr error
	for i := range cmsgs {
		if cmsgs[i].Header.Type != unix.SCM_RIGHTS {
			// Not a rights message (e.g. the SCM_CREDENTIALS that rides
			// the same handover message); receivedUcreds handles it.
			continue
		}
		rights, rerr := unix.ParseUnixRights(&cmsgs[i])
		if rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		fds = append(fds, rights...)
	}
	return fds, firstErr
}

// receivedUcreds extracts the SCM_CREDENTIALS control message (the
// sender's pid, uid, gid) from a control message's oob bytes, if
// present. The pid is in the pid namespace of the reading process: the
// kernel records the sender's struct pid in the skb and translates it
// (pid_vnr) to the reader's namespace at read time, so a sender in a
// container pid namespace still yields a pid to look up in the /proc
// flue reads (the host's, when flue runs on the host).
func receivedUcreds(oob []byte) (*unix.Ucred, error) {
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	for i := range cmsgs {
		if cred, perr := unix.ParseUnixCredentials(&cmsgs[i]); perr == nil {
			return cred, nil
		}
	}
	return nil, errors.New("no SCM_CREDENTIALS in control message")
}

// sessionIDFromStatLine extracts the session id (field 6) from a
// /proc/<pid>/stat line. The comm field (2) is wrapped in parentheses
// but may itself contain spaces and parentheses, so the parse starts
// after the last `)`: the remaining fields are state, ppid, pgrp,
// session, ...
func sessionIDFromStatLine(line string) (int, error) {
	idx := strings.LastIndexByte(line, ')')
	if idx < 0 {
		return 0, errors.New("no closing paren")
	}
	fields := strings.Fields(line[idx+1:])
	if len(fields) < 4 {
		return 0, errors.New("too few fields after comm")
	}
	return strconv.Atoi(fields[3])
}

// sessionIDOfPID reads the session id of pid from /proc/<pid>/stat.
// Session ids are comparable between processes only as long as they
// are read from the same /proc: field 6 is the session leader's pid
// translated to the namespace in which /proc is mounted, so a broker's
// and a client's values read from the same /proc (e.g. the host's) are
// directly comparable even when the two processes live in container
// pid namespaces.
func sessionIDOfPID(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	return sessionIDFromStatLine(string(data))
}

// sessionCheckMode decides what flue does when the client session check
// is, or is not, enabled (see the file header for the rationale).
type sessionCheckMode int

const (
	// sessionCheckAdvisory: best effort. The check is on when the
	// broker's session is private, off otherwise; its state is logged at
	// info at handover. Used for an injected broker without a tty.
	sessionCheckAdvisory sessionCheckMode = iota
	// sessionCheckRequired: flue knows it ran an injected broker with a
	// tty (`flue enter` from a terminal) — the interactive session the
	// check exists to protect. If the check cannot be enabled, the
	// session is aborted (fail closed).
	sessionCheckRequired
	// sessionCheckOff: the broker was not injected — an enter with
	// --no-inject-broker, or the flue exec passthrough — so it is
	// untrusted code that could hand over a listener from any session it
	// controls; the session it reports is not a trustworthy reference,
	// and the check is never used (a warning is logged).
	sessionCheckOff
)

// sessionCheckActive reports whether the client session check is in
// effect: the broker's session must be known and it must have been
// private at handover time (see sessionIsPrivate). In
// sessionCheckOff mode it is false even in principle, because the
// privacy scan is skipped (see receiveListener); in
// sessionCheckRequired mode a false return is a session failure, handled
// by the caller.
func sessionCheckActive(pol *policy) bool {
	return pol.brokerSession > 0 && pol.sessionPrivate
}

// sessionIsPrivate reports whether no process other than brokerPID is
// alive in session (both ids in the namespace of the /proc flue reads).
//
// A session's members are the leader and its (transitive) descendants:
// membership is inherited by fork, and setsid only ever creates a new
// session — a process can never join an existing one. So a session
// whose only live member at handover time — before the broker forks its
// wrapped command — is the broker itself can later contain only the
// broker and its descendants. That is exactly the "legitimate client"
// set the session check protects: a client connecting from the session
// this exec created, and nothing from another exec or the container's
// init tree.
//
// The gate the code previously used — the broker is the session leader
// (session id == its own pid) — is a special case of this: a leader
// with no other live member is private. But a per-exec session whose
// leader has already exited is private without the broker being a
// leader, and that is what recent podman/crun setups produce for every
// exec, with or without a tty (the exec'd process is a member, not the
// leader, of a session a short-lived helper created), so the leader
// test left the check off there. A session with other live members —
// the container's init session, where older runtimes place an exec
// without its own session — is not private, and the check stays off:
// every process in the container would look "the same session".
//
// The scan reads the /proc directory and every process's stat line
// once; it runs once per session, at handover, so the cost is a few
// milliseconds. A member that forks and exits while the scan runs,
// with pid wraparound placing the child at an already-scanned pid,
// would be missed — theoretical: it needs a second process in the
// session, which is the very thing the scan excludes.
func sessionIsPrivate(brokerPID, session int) (bool, error) {
	if brokerPID <= 0 || session <= 0 {
		return false, nil
	}
	d, err := os.Open(procRoot)
	if err != nil {
		return false, err
	}
	defer d.Close()
	for {
		entries, err := d.Readdir(256)
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == brokerPID {
				continue // not a process directory, or the broker itself
			}
			other, err := sessionIDOfPID(pid)
			if err != nil {
				continue // process exited during the scan; a reused pid
				// cannot land in this session (membership is by fork)
			}
			if other == session {
				return false, nil
			}
		}
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// sessionCheckOffReason explains why the session check is not in effect
// for this session (for the state log at handover).
func sessionCheckOffReason(pol *policy) string {
	switch {
	case pol.brokerPID == 0:
		return "no broker credentials in the handover message (SO_PASSCRED not in effect, or the sender's pid is not visible in flue's pid namespace)"
	case pol.brokerSession == 0:
		return "the broker's session id could not be read from /proc (its pid is not visible in flue's pid namespace)"
	case !pol.sessionPrivateKnown:
		return "the broker's session could not be checked for privacy (the /proc scan failed)"
	default:
		return fmt.Sprintf("the broker's session %d is shared with other live processes (not private to this session)", pol.brokerSession)
	}
}

// peerSessionID returns the session id of the peer of a connected Unix
// socket. getsockopt(SO_PEERCRED) yields the peer's pid in flue's pid
// namespace — the kernel records the connecting task at connect time,
// so it works on accepted connections without SO_PASSCRED — and the
// session is read from /proc (sessionIDOfPID).
//
// The pid is only as trustworthy as the window between connect and the
// /proc read: if the peer exits and its pid is reused before the read,
// the stat line describes a different process. SO_PEERPIDFD (Linux
// 6.5+) hands out a pidfd for the peer as recorded at connect time:
// the kernel keeps a reference to the original task, not a pid number,
// so a reused pid cannot be confused with it. After the read, the
// pidfd is checked for liveness (pidfd_send_signal(pidfd, 0): 0 or
// EPERM means alive, ESRCH means gone). The read-then-check order is
// what makes it sound: if the pidfd is alive at check time, the
// original peer was alive across the whole read window, so the session
// read is the client's; a peer that is gone (or whose session could
// not be read) is an error, not a guess, and the caller fails closed.
// If the getsockopt itself fails with ENOPROTOOPT the kernel predates
// the option and the check is skipped: the window is tiny, and an
// attacker cannot easily arrange for a reused pid to land in the
// user's session. Any other getsockopt failure (the peer's task is
// already gone, or none was recorded) means the read cannot be
// verified: refuse.
func peerSessionID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a Unix connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var ucred *unix.Ucred
	err = rc.Control(func(fd uintptr) {
		ucred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if ucred == nil || ucred.Pid <= 0 {
		return 0, errors.New("no peer credentials")
	}
	sess, err := sessionIDOfPID(int(ucred.Pid))
	if err != nil {
		return 0, err
	}
	// Liveness check (see above): if the peer's task is gone, the
	// /proc read above may describe a reused pid, so refuse.
	if _, lerr := peerPidfdLiveness(rc); lerr != nil {
		return 0, lerr
	}
	return sess, nil
}

// peerPidfdLiveness checks the liveness of the peer of a Unix
// connection via SO_PEERPIDFD (see peerSessionID): the pidfd is a
// reference to the peer's task as recorded at connect time, so a
// reused pid cannot be confused with it. It reports (true, nil) when
// the peer is alive, and also when the kernel predates the option
// (getsockopt fails with ENOPROTOOPT: the /proc read stands,
// best-effort); any other outcome (the peer's task is gone, or none
// was recorded) is an error and the caller fails closed.
func peerPidfdLiveness(rc syscall.RawConn) (bool, error) {
	var pidfd int
	var gerr error
	cerr := rc.Control(func(fd uintptr) {
		pidfd, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	})
	if cerr != nil {
		return false, fmt.Errorf("pidfd getsockopt failed: %w", cerr)
	}
	if gerr != nil {
		if e, ok := gerr.(unix.Errno); ok && (e == unix.ENOPROTOOPT || e == unix.EOPNOTSUPP) {
			return true, nil
		}
		return false, fmt.Errorf("peer process no longer verifiable: %v", gerr)
	}
	// The kernel creates the SO_PEERPIDFD fd with O_CLOEXEC
	// (__pidfd_prepare reserves the fd and its anon inode with
	// O_RDWR | O_CLOEXEC; verified in v6.5 through 7.2, including
	// the pidfs rewrite) and this close makes it airtight — a pidfd
	// must never reach a child process. Keep that in mind when a
	// child is added here (the desktop endpoint).
	defer unix.Close(pidfd)
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil && err != unix.EPERM {
		return false, fmt.Errorf("peer exited before its session could be verified: %w", err)
	}
	return true, nil
}

// clientSessionAllowed decides one session comparison: brokerSession is
// the reference (0 = check not active), clientSession the client's value
// (0 = unknown). Unknown client or a different session refuse; the
// equal case allows.
func clientSessionAllowed(brokerSession, clientSession int) bool {
	if brokerSession <= 0 {
		return true
	}
	return clientSession == brokerSession
}

// checkClientSession enforces the session check on an accepted client
// connection and reports whether it may be proxied. It fails closed in
// both directions of the comparison: a client whose session cannot be
// determined (not a Unix socket, no peer credentials, or a pid that
// vanished before /proc could be read) and a client in a different
// session are both refused. In required mode an inactive check is also
// refused — a belt-and-braces; receiveListener aborts the session
// before this can matter.
func checkClientSession(client net.Conn, pol *policy) bool {
	if !sessionCheckActive(pol) {
		return pol.sessionCheckMode != sessionCheckRequired
	}
	sess, err := peerSessionID(client)
	if err != nil {
		// Rate-limited: a container process can connect in a loop to
		// flood the user's terminal (lograte.go).
		pol.refusals.reject(slog.LevelWarn, "flue: client session unknown; refusing", "err", err)
		return false
	}
	if !clientSessionAllowed(pol.brokerSession, sess) {
		pol.refusals.reject(slog.LevelWarn, "flue: client session differs from the broker's; refusing",
			"client-session", sess, "broker-session", pol.brokerSession)
		return false
	}
	return true
}

// closedChannel returns an already-closed channel, used as a sentinel
// acceptDone when no listener was received.
func closedChannel() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// handleClient proxies one client connection to the agent: it dials a
// fresh upstream connection, wraps it with a filter carrying the session's
// policy, and runs agent.ServeAgent until the client disconnects.
func handleClient(client net.Conn, pol *policy) {
	defer closeQuiet(client)
	client = &idleConn{Conn: client, timeout: idleTimeout}

	upstream, err := dialAgent(pol.sock)
	if err != nil {
		log.Warn("flue: dial upstream", "err", err)
		return
	}
	defer closeQuiet(upstream)

	// ServeAgent returns when the client connection ends; I/O errors there
	// are normal (peer closed), so the result is not checked.
	_ = agent.ServeAgent(&agentFilter{
		upstream: agent.NewClient(upstream),
		pol:      pol,
	}, client)
}

// idleConn re-arms a deadline before every Read and Write, so a connection
// that goes quiet for longer than timeout fails its next I/O and ends the
// handler instead of pinning it forever.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

// dialAgent connects to the host's real SSH agent at sock, the path the
// root PersistentPreRun resolved from --sock or $SSH_AUTH_SOCK.
func dialAgent(sock string) (net.Conn, error) {
	return net.DialTimeout("unix", sock, dialTimeout)
}
