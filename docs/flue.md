# flue (host-side binary)

`cmd/flue` — the host-side half of the bridge. The file header in
`main.go` is the authoritative overview; this doc covers the mechanics an
agent needs when changing the code.

## Modes

flue is a cobra CLI with two subcommands. The global flags (`--sock`,
`--allow-key`, `--log-level`) are persistent on the root and go before the
subcommand; a subcommand's arguments follow the subcommand name, and a `--`
separates them from the target command so the target's own flags pass
through verbatim (pflag, unlike stdlib flag, keeps scanning for flags after
positionals — which is why the `--` is needed for commands with flags).

- **exec** — `flue exec [--] <command...>`: run any command that receives
  the control socketpair (typically `podman exec --preserve-fds 1 ...
  flue-broker`, or a wrapper that preserves fd 3).
- **enter** — `flue enter <container> [--] [command...]`: builds and runs
  `podman exec [-ti] --preserve-fds 1 -e SSH_AUTH_SOCK=/tmp/flue.sock
  <container> flue-broker [command...]`.
  - The in-container socket path is the const `enterSocket`
    (`/tmp/flue.sock`) and **must match the broker's `-sock` default** —
    guarded by `TestEnterSocketMatchesBrokerDefault`.
  - The command defaults to `defaultEnterCmd` (`/bin/sh`).
  - A tty is allocated only when stdin is a tty (`stdinIsTTY`, via the
    TCGETS ioctl — x/sys v0.49.0 has no `unix.IsTerminal`).

Both subcommands share the socketpair machinery in `runChildWithAgent`,
which returns the child's exit code. The root `PersistentPreRun` parses the
log level and resolves the agent socket path once, for whichever subcommand
runs; a bare `flue` (no subcommand) is a usage error.

## Control channel

- `SOCK_SEQPACKET` socketpair; end B is passed to the child as fd 3
  (`exec.Command.ExtraFiles`), end A stays on the host.
- It carries exactly one message: the broker's listener fd (SCM_RIGHTS),
  read by `receiveListener`.
- The connection stays open until the broker is gone (so the broker never
  sees a premature "host gone") and is closed after `cmd.Wait()`.

## Lifetime

flue blocks on `cmd.Wait()` of its child (the `podman exec` running the
broker), **not** on control-channel EOF — the netpoller does not
reliably deliver EOF for a closed socketpair end in all environments.
The accept loop runs in a background goroutine, stopped by closing the
listener.

- Standalone mode: the broker outlives the child, so flue runs until
  signalled.
- Wrapper mode: the broker exits when the wrapped command ends, so flue
  returns that command's exit status.

## Agent proxy

- Each accepted client connection gets a fresh `net.Dial("unix",
  SSH_AUTH_SOCK)` upstream connection, wrapped in an `agentFilter` that
  implements `agent.ExtendedAgent`.
- Policy (mirrors the per-operation table in the README):
  - `List`: forwarded, but only keys matching `--allow-key` (repeatable,
    SHA256 fingerprint) are returned; all keys when the flag is absent.
  - `Sign` / `SignWithFlags`: forwarded only for allowed keys.
  - `Add`, `Remove`, `RemoveAll`, `Lock`, `Unlock`, `Signers`: always
    denied.
  - `Extension`: only `session-bind@openssh.com` is forwarded.
- Per-request agent trace is emitted at `debug` (`--log-level debug`).

## Dependencies

Runtime: `golang.org/x/sys` (unix syscalls) + `golang.org/x/crypto` (ssh
agent protocol) + `github.com/spf13/cobra` (CLI: flags, subcommands,
help/completion). Test-only: `go.uber.org/goleak` (goroutine leak
detection) + `github.com/google/go-cmp` (readable test diffs).

Logging is `log/slog` (stdlib) to stderr, set by `--log-level` (parsed in
the root `PersistentPreRun`). Fatal setup errors (no agent socket, bad log
level, socketpair/control-fd/child-start failures) are plain text on
stderr and exit 1, via `fatalf` — not slog records. Flag and argument
errors are reported by cobra and exit 1 from `main`.
