# flue (host-side binary)

`cmd/flue` — the host-side half of the bridge. The file header in
`main.go` is the authoritative overview; this doc covers the mechanics an
agent needs when changing the code.

## Modes

flue is a cobra CLI with four subcommands: `exec`, `enter`, `broker`
(itself the group of the three sub-subcommands below), and `version`
(informational). The global flags (`--sock`, `--allow-key`,
`--allow-key-type`, `--log-level`, `--log-file`) are persistent on the
root and go before the subcommand; a subcommand's arguments follow the
subcommand name, and a `--` separates them from the target command so the
target's own flags pass through verbatim (pflag, unlike stdlib flag, keeps
scanning for flags after positionals — which is why the `--` is needed for
commands with flags).

- **exec** — `flue exec [--] <command...>`: run any command that receives
  the control socketpair (typically `podman exec --preserve-fds 1 ...
  flue-broker`, or a wrapper that preserves fd 3).
- **enter** — `flue enter [container] [--] [command...]`: builds and runs
  `podman exec [-ti] --preserve-fds <n> <container> <broker>
  [command...]`, where `<broker>` is `/proc/self/fd/4` (injected memfd,
  `--preserve-fds 2`) or the image's `flue-broker` (`--preserve-fds 1`).
  - The container defaults to `defaultEnterContainer` (`snugmo`), and the
    command to `defaultEnterCmd` (`/bin/sh`) unless `$FLUE_ENTER_CMD` is
    set and non-blank, in which case it is that value split on whitespace
    (resolved by `enterDefaultCmd`); an explicit command always wins. So
    a bare `flue enter` runs a shell in snugmo.
  - A `--` separator (recovered from the raw process arguments by
    `enterDashDash`, since pflag strips it before Run) takes precedence
    over that split: the tokens after it are the command, and the single
    positional argument before it — if any — names the container, so
    `flue enter -- bash` runs bash in snugmo, not the default command in
    a container named bash. A trailing separator (nothing after it) is a
    no-op; more than one positional before it is a usage error.
  - A tty is allocated only when stdin is a tty (`stdinIsTTY`, via the
    TCGETS ioctl — x/sys v0.49.0 has no `unix.IsTerminal`).
  - **Memfd injection is the default** (`--inject-broker` defaults on):
    instead of the image's `flue-broker`, flue hands the broker in a
    `memfd` and the container execs it via `/proc/self/fd/4` — see
    [embedded-broker.md](embedded-broker.md) for the mechanics. Disable
    it with
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
- **broker** — the group in `cmd/flue/broker.go` that gets the embedded
  `flue-broker` (see **Embedded broker** below) out as files:
  `executable <filename>` writes the binary (mode 0755, byte-identical
  to the built broker), `sha256` prints the broker's digest as a bare
  hex digest, and `sigstore-bundle [filename]` writes the signature
  bundle (mode 0644 — or to stdout with no filename, or `-`) and warns
  without failing when it is the local placeholder. None of the
  sub-subcommands needs an agent socket, and none takes flags. Usage,
  offline verification of a recovered broker, and the injection that
  runs the same bytes in memory are in
  [embedded-broker.md](embedded-broker.md).

The two agent subcommands share the socketpair machinery in
`runChildWithAgent`, which returns the child's exit code. The root
`PersistentPreRun` resolves the log level (a set `--log-level`, else
`$FLUE_LOG_LEVEL`, else `info`) and the log destination (a set
`--log-file`, else stderr) for every subcommand, but
resolves the agent socket path only for the ones annotated with
`needsAgentAnnotation` (`exec`, `enter`) — so the `broker`
sub-subcommands run without `SSH_AUTH_SOCK` being set. A bare `flue`
(no subcommand) is a usage error.

## Control channel

- `SOCK_SEQPACKET` socketpair; end B is passed to the child as fd 3
  (`exec.Command.ExtraFiles`), end A stays on the host.
- It carries exactly one message: the broker's listener fd (SCM_RIGHTS),
  read by `receiveListener`. The host end has `SO_PASSCRED` enabled, so
  the broker's credentials (`SCM_CREDENTIALS` — its pid in flue's pid
  namespace: the kernel translates the sender's pid to the reader's
  namespace, so for a flue on the host that is the host's) ride the same
  message, and `receiveListener` turns
  that pid into the broker's session id (`policy.brokerSession`), the
  reference the client session check compares against.
- The received fd is validated before use: `getsockopt` on the raw fd must
  report `AF_UNIX` / `SOCK_STREAM` / `SO_ACCEPTCONN=1`, and the wrapped
  result must be a `*net.UnixListener`. Anything else (e.g. a TCP listener
  from a host-networked container) is logged, closed, and rejected — the
  accept loop only ever runs on a Unix listener.
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
- The session's policy (allowlists + socket path) is a single `policy`
  value built once in `runChildWithAgent`; each per-client `agentFilter`
  shares it read-only.
- Policy (mirrors the per-operation table in the README):
  - `List`: forwarded, but only keys allowed by `--allow-key`
    (repeatable, SHA256 fingerprint) and `--allow-key-type` (repeatable,
    wire type name) are returned; all keys when the flags are absent.
  - `Sign` / `SignWithFlags`: forwarded only for keys allowed by both
    lists.
  - `Add`, `Remove`, `RemoveAll`, `Lock`, `Unlock`, `Signers`: always
    denied.
  - `Extension`: only `session-bind@openssh.com` is forwarded.
  - A certificate matches by both its certificate type and its
    underlying key's type, and its fingerprint is the underlying
    key's, so `--allow-key-type` can admit or deny certificates
    independently of static keys; a key of an unknown or unparsable
    type matches no type list and is denied when `--allow-key-type`
    is set (fail closed).
  - A key an allowlist denies is logged with a `reason` field naming
    the violated list(s) by flag name (`allow-key`, `allow-key-type`)
    — so the trace says which flag to adjust. The error returned to
    the client is deliberately the same either way.
- Per-request agent trace is emitted at `debug` (`--log-level debug`):
  an op about a specific key carries the key type and its fingerprint
  (for a certificate, the certificate's own type), other ops a summary.

## Session check

- At handover, the broker's credentials (`SCM_CREDENTIALS`, see the
  control channel above) give flue the broker's pid (in flue's pid
  namespace) and session id (`/proc/<pid>/stat` field 6 — the session
  leader's pid in the namespace of `/proc` — via `sessionIDOfPID`).
- The check is active when the broker's session is **private**: its
  only live member at handover time (before the broker forks its
  wrapped command) is the broker itself (`sessionIsPrivate` scans
  `/proc` once to decide). Session membership is inherited by fork
  only — a process can never join an existing session — so a private
  session can later contain only the broker and its descendants, which
  is exactly the "legitimate client" set. A session is private when
  this exec created it: the broker is its leader (older runtimes, with
  `-t`: the pty setup does the `setsid` in the exec'd process), or it
  was led by a short-lived per-exec helper that has already exited
  (recent runtimes, with or without a tty). An exec whose process
  shares a session with the container's other processes — the
  container's init session, the classic no-tty case — is not private,
  and the check is off.
- The check's **mode** depends on how the broker was started
  (`sessionCheckMode`):
  - **required** — `flue enter` from a terminal (injected broker +
    tty): the check must be enabled; if it cannot be (no broker
    credentials in the handover, the broker's session unreadable, or
    its session not private), flue aborts the session — closing the
    control connection, which makes the broker kill the wrapped
    command — rather than serve a session without the second gate
    (fail closed).
  - **off** — `--no-inject-broker` or the `flue exec` passthrough: the
    broker is untrusted code that could hand over a listener from any
    session it controls, so the session it reports is not a trustworthy
    reference; the check is never used, a warning says why, and the
    privacy scan is skipped.
  - **advisory** (the default) — an injected broker without a tty:
    the check is on when the broker's session is private, off
    otherwise.
  The mode is an escape hatch on `enter`:
  `--session-check=required|advisory|off` overrides it (e.g. `off`
  keeps injection but opts out of the required check on a runtime
  whose execs share a session). An explicit choice is logged at
  `info`; the default is logged at handover.
- The check's state is logged once at handover: at `info` when the
  mode is advisory (`session check on` / `session check off:
  <reason>`), a `warn` with the reason in off mode, and an `error`
  that aborts the session in required mode when the check cannot be
  enabled.
- Each accepted connection is checked before it is proxied:
  `getsockopt(SO_PEERCRED)` on the accepted connection yields the
  client's pid in flue's pid namespace — the kernel records it at
  connect time and translates it to the reader's namespace, so a client
  in a container pid namespace still yields a usable pid — and its
  session id is read the same way as the broker's. A client
  whose session is unknown (no peer credentials, or the pid already
  gone) or different from the broker's is refused: a warn is logged
  with both session ids and the connection is closed. Every failure
  direction closes. Refusal warnings — session-check refusals and the
  live-connection cap ("too many clients") — are rate-limited
  (`refusalLimiter`, `lograte.go`): the first of a burst is logged in
  full, the rest are summarized once every 30 s
  (`flue: client refusals`, `more=N`) and whatever is left is flushed
  when the session ends — a process that connects in a loop can count
  but not flood the user's terminal (and `--log-file` keeps all of it
  out of the terminal anyway).
- Against the pid-reuse race (the peer exits and its pid is reused
  before the `/proc` read), `SO_PEERPIDFD` (Linux 6.5+) hands out a
  pidfd for the peer **as recorded at connect time**: the kernel keeps
  a reference to the original task, not a pid number, so a reused pid
  cannot be confused with it. After reading `/proc`, `peerSessionID`
  checks the pidfd is still alive (`pidfd_send_signal(pidfd, 0)` —
  `0` or `EPERM` means alive, `ESRCH` means gone); the read-then-check
  order is what makes it sound: if the pidfd is alive at check time,
  the original peer was alive across the whole read window, so the
  session read is the client's; a gone peer is refused. On kernels
  without the option the getsockopt fails with `ENOPROTOOPT` and the
  check is skipped (the `/proc` read stands best-effort: the window is
  tiny, and an attacker cannot easily arrange for a reused pid to land
  in the user's session); any other getsockopt failure (the peer's
  task is already gone, or none was recorded) refuses the connection.

## Embedded broker

- `cmd/flue/embed.go` declares `//go:embed flue-broker.bin`: the complete
  `flue-broker` binary built when flue was built. go:embed stores the
  file's bytes verbatim, so the embedded copy is 100% identical to the
  build source, and `flue broker executable` is how that file comes
  back out as a file; `flue enter` uses the same bytes, by default, to
  run the broker entirely in memory (see the memfd injection bullet
  below and [embedded-broker.md](embedded-broker.md)).
- `cmd/flue/embed.go` also declares `//go:embed flue-broker.sigstore`:
  the broker's sigstore bundle, stored verbatim like the binary. For a
  release `flue` it is the bundle the release workflow's
  `cosign sign-blob --bundle` wrote over this very broker file
  (self-contained — signature, certificate, chain — so
  `cosign verify-blob <broker> --bundle <bundle>` needs no registry
  or network); for a locally signed build it is whatever bundle the
  user signed with (`cosign sign-blob --key <key> --bundle
  cmd/flue/flue-broker.sigstore cmd/flue/flue-broker.bin`); for a
  bare local build it is a copy of the committed placeholder
  template, and `broker sigstore-bundle` says so when it writes it out.
- **Memfd injection** (`cmd/flue/inject.go`): `flue enter` runs the
  broker entirely in memory by default (`--no-inject-broker` falls back
  to the image's `flue-broker`). `injectBrokerMemfd` creates a sealed
  memfd (named `flue-broker`, `MFD_EXEC | MFD_CLOEXEC |
  MFD_ALLOW_SEALING`, written with the embedded bytes and sealed
  read-only/fixed-size so a container process cannot rewrite the running
  broker), passes it to the container as fd 4, and the container execs
  `/proc/self/fd/4` — the full mechanics, including the `EINVAL`
  fallback for pre-6.3 kernels (no `MFD_EXEC`) and the hard error on a
  system-level refusal such as `vm.memfd_noexec=2`, are in
  [embedded-broker.md](embedded-broker.md). The same doc covers how the
  injection interacts with the session check (injected = trusted = the
  check stays on; `--no-inject-broker` = off).
- **Build order**: both embed sources must exist before flue is
  compiled — build `flue-broker` first, to `cmd/flue/flue-broker.bin`
  (the AGENTS.md Build/Test commands and the Containerfile all do
  this), and have a bundle at `cmd/flue/flue-broker.sigstore` —
  signed by the release workflow or a local user, else a copy of the
  committed placeholder template. If either file is missing,
  compiling `cmd/flue` fails with "no matching files found": a
  deliberate hard failure, so a flue without its broker (or its
  bundle) is never built silently.
- **`broker executable`** (`cmd/flue/extract.go`): checks the embedded
  blob's ELF magic first (a non-broker embed source fails at extraction
  time, not in a container later), then writes atomically — a temp file
  in the target directory, `chmod 0755`, rename — so the target is
  never left half-written. It refuses a directory as target.
- **`broker sigstore-bundle`** (`cmd/flue/bundle.go`): with a
  filename, writes the embedded bundle with the same atomic write as
  `executable` (shared `writeFileAtomic`), mode 0644; with no filename
  (the default) or `-`, writes it to `cmd.OutOrStdout()` instead. Both
  forms call `warnPlaceholderBundle` first: it detects the local
  placeholder by its JSON `kind` field (`FlueLocalPlaceholder`) and
  logs a warning that the bundle will not verify the extracted broker
  with cosign — the output still happens, so the command stays
  scriptable, and the warning goes to stderr (or `--log-file`), never
  into the bundle's destination, so the stdout form stays pipe-clean.
- **Architecture**: the embedded broker is built for the same
  architecture as this flue binary (one source tree, one build). The
  Containerfile's build stage builds the broker to the embed path before
  each architecture's flue, so a flue from the image embeds a broker
  built for the image's architecture — the image ships no separate
  broker, `flue broker executable` recovers the embedded one.

## Dependencies

Runtime: `golang.org/x/sys` (unix syscalls) + `golang.org/x/crypto` (ssh
agent protocol) + `github.com/spf13/cobra` (CLI: flags, subcommands,
help/completion) + stdlib `go:embed` (the embedded broker and its
sigstore bundle — no dependency). Test-only: `go.uber.org/goleak`
(goroutine leak detection) + `github.com/google/go-cmp` (readable
test diffs).

Logging is `log/slog` (stdlib) to stderr — or to the file named by
`--log-file` (created if needed, opened in append mode, mode 0600),
useful to keep refusal warnings off the user's terminal — set by
`--log-level` (else `$FLUE_LOG_LEVEL`, else `info`; both parsed in the
root `PersistentPreRun`). Fatal setup errors (no agent socket, bad log
level, socketpair/control-fd/child-start failures) are plain text on
stderr and exit 1, via `fatalf` — not slog records. Flag and argument
errors are reported by cobra and exit 1 from `main`.
