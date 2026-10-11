# flue (host-side binary)

`cmd/flue` — the host-side half of the bridge. The file header in
`main.go` is the authoritative overview; this doc covers the mechanics an
agent needs when changing the code.

## Modes

flue is a cobra CLI with three subcommands. The global flags (`--sock`,
`--allow-key`, `--log-level`) are persistent on the root and go before the
subcommand; a subcommand's arguments follow the subcommand name, and a `--`
separates them from the target command so the target's own flags pass
through verbatim (pflag, unlike stdlib flag, keeps scanning for flags after
positionals — which is why the `--` is needed for commands with flags).

- **exec** — `flue exec [--] <command...>`: run any command that receives
  the control socketpair (typically `podman exec --preserve-fds 1 ...
  flue-broker`, or a wrapper that preserves fd 3).
- **enter** — `flue enter <container> [--] [command...]`: builds and runs
  `podman exec [-ti] --preserve-fds <n> <container> <broker>
  [command...]`, where `<broker>` is `/proc/self/fd/4` (injected memfd,
  `--preserve-fds 2`) or the image's `flue-broker` (`--preserve-fds 1`).
  - The command defaults to `defaultEnterCmd` (`/bin/sh`).
  - A tty is allocated only when stdin is a tty (`stdinIsTTY`, via the
    TCGETS ioctl — x/sys v0.49.0 has no `unix.IsTerminal`).
  - **Memfd injection is the default** (`--inject-broker` defaults on):
    instead of the image's `flue-broker`, flue hands the broker in a
    `memfd` and the container execs it via `/proc/self/fd/4` — see the
    memfd injection bullet under **Embedded broker**. Disable it with
    `--no-inject-broker`, `--inject-broker=false`, or
    `FLUE_INJECT_BROKER=0` (a set flag wins over the env var). pflag,
    unlike stdlib flag, does not auto-generate `--no-<flag>` forms, so
    `--no-inject-broker` is a real companion flag registered in `init`
    (`negBoolValue` bound to the same variable, with `NoOptDefVal "true"`
    so the bare form disables without consuming an argument).
  - The in-container socket path is not known to the host: the broker
    generates a unique per-session path (XDG_RUNTIME_DIR → /run/user/<uid>
    → /run → /tmp) unless `-sock` is given, and exports it to the wrapped
    command as `SSH_AUTH_SOCK` (see the broker doc).
- **extract-broker** — `flue extract-broker <filename>`: writes the
  embedded `flue-broker` (see Embedded broker below) to `<filename>`,
  mode 0755, byte-identical to the built broker. It takes no flags.

The two agent subcommands share the socketpair machinery in
`runChildWithAgent`, which returns the child's exit code. The root
`PersistentPreRun` resolves the log level (a set `--log-level`, else
`$FLUE_LOG_LEVEL`, else `info`) for every subcommand, but
resolves the agent socket path only for the ones annotated with
`needsAgentAnnotation` (`exec`, `enter`) — so `extract-broker` runs
without `SSH_AUTH_SOCK` being set. A bare `flue` (no subcommand) is a
usage error.

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

## Embedded broker

- `cmd/flue/embed.go` declares `//go:embed flue-broker.bin`: the complete
  `flue-broker` binary built when flue was built. go:embed stores the
  file's bytes verbatim, so the embedded copy is 100% identical to the
  build source, and `flue extract-broker` (below) is how that file comes
  back out as a file; `flue enter` uses the same bytes, by default, to
  run the broker entirely in memory (see the memfd injection bullet below).
- **Memfd injection** (`cmd/flue/inject.go`): `flue enter` runs the broker
  entirely in memory by default (`--no-inject-broker` falls back to the
  image's `flue-broker`). `injectBrokerMemfd` creates a memfd
  named `flue-broker` with `MFD_EXEC | MFD_CLOEXEC` and writes the
  embedded bytes into it; the memfd is passed to the container as fd 4
  (`--preserve-fds 2`, control socketpair stays on fd 3) and the container
  execs `/proc/self/fd/4`, so the broker never touches the container's
  filesystem — it works in a container with no writable filesystem and no
  `flue-broker` in the image. `MFD_EXEC` (Linux ≥ 5.17) makes the memfd
  executable via `/proc/self/fd/<n>` even in MDWE-hardened processes; if
  the kernel lacks it, flue falls back to a plain memfd, which ordinary
  (non-MDWE) processes can still exec. In `/proc/self/fd` the injected
  broker shows up as `memfd:flue-broker (deleted)`.
- **Build order**: the embed file must exist before flue is compiled —
  build `flue-broker` first, to `cmd/flue/flue-broker.bin` (the AGENTS.md
  Build/Test commands, `build.sh`, and the Containerfile all do this).
  If the file is missing, compiling `cmd/flue` fails with "no matching
  files found": a deliberate hard failure, so a flue without its broker
  is never built silently.
- **`extract-broker`** (`cmd/flue/extract.go`): checks the embedded blob's
  ELF magic first (a non-broker embed source fails at extraction time, not
  in a container later), then writes atomically — a temp file in the
  target directory, `chmod 0755`, rename — so the target is never left
  half-written. It refuses a directory as target.
- **Architecture**: the embedded broker is built for the same
  architecture as this flue binary (one source tree, one build). The
  Containerfile builds the broker to the embed path and copies the same
  file to `/usr/local/bin/flue-broker`, so a flue from the image extracts
  a broker identical to the image's own, for the image's architecture.

## Dependencies

Runtime: `golang.org/x/sys` (unix syscalls) + `golang.org/x/crypto` (ssh
agent protocol) + `github.com/spf13/cobra` (CLI: flags, subcommands,
help/completion) + stdlib `go:embed` (the embedded broker — no
dependency). Test-only: `go.uber.org/goleak` (goroutine leak
detection) + `github.com/google/go-cmp` (readable test diffs).

Logging is `log/slog` (stdlib) to stderr, set by `--log-level` (else `$FLUE_LOG_LEVEL`, else `info`; parsed in
the root `PersistentPreRun`). Fatal setup errors (no agent socket, bad log
level, socketpair/control-fd/child-start failures) are plain text on
stderr and exit 1, via `fatalf` — not slog records. Flag and argument
errors are reported by cobra and exit 1 from `main`.
