# flue-broker (container-side binary)

`cmd/flue-broker` — the container-side half of the bridge. Deliberately
thin: after handing the listener to the host it performs no protocol
parsing, no key filtering, and never reads from or writes to client
connections. The file header in `main.go` is the authoritative overview.

## Modes

- **Standalone** (no arguments): hands the listener to the host, blocks
  until the control channel reports the host gone (EOF) or a termination
  signal (SIGTERM/SIGHUP/SIGINT) arrives, removes the socket, and exits.
- **Wrapper** (`flue-broker [cmd] [args...]`): hands the listener over,
  then runs the command as a child process with `SSH_AUTH_SOCK` pointed
  at the broker's socket, and exits with the child's exit status. The
  child's stdio is wired straight through, so an interactive shell works.
  Host departure (detected best-effort via the control channel) kills the
  child and the broker exits 1. SIGTERM/SIGHUP reaching the broker are
  forwarded to the child; SIGINT is swallowed (on a tty the child already
  gets its own copy).

## Mechanics

- Listens on `-sock`, removing a stale socket first. When `-sock` is
  unset (or set to the empty value) a unique per-session path is
  generated instead, so pin `-sock` for standalone mode.
- Names itself `flue-broker` (`PR_SET_NAME` at startup, so `ps` and
  `/proc/<pid>/comm` show it that way) — this is what makes an injected
  broker, exec'd from the memfd as `/proc/self/fd/4`, readable in `ps`.
- **Socket path**: when `-sock` is unset (or empty), the broker
  generates `flue-<8hex>.sock` under the best available runtime
  directory (`$XDG_RUNTIME_DIR` if set and writable, then
  `/run/user/<uid>`, then `/run` — the first writable of those wins —
  else `/tmp` as an unconditional last resort)
  and — in wrapper mode — exports it to the wrapped command as
  `SSH_AUTH_SOCK`. Standalone mode exports nothing, so the caller must
  pin `-sock` and set `SSH_AUTH_SOCK` itself.
- The preserved control fd comes from `-fd` (default `3` — the first fd
  after stdin/stdout/stderr; must be ≥ 3). At startup the broker verifies
  the fd is open and a `SOCK_SEQPACKET` socket, and exits with a
  diagnostic otherwise: running the exec without `--preserve-fds 1`
  leaves fd 3 open as *something else* (a pipe, a file, …), and without
  the check that would fail only later, deep in the SCM_RIGHTS handoff,
  as an opaque "socket operation on non-socket". If the calling shell
  holds other open fds, use `-fd` to target the right one.
- **SCM_RIGHTS listener-fd handoff**: the broker's listening fd is passed
  to the host over the control socketpair using `unix.UnixRights` +
  `WriteMsgUnix` exactly once at startup. The control channel is one-shot;
  there is no per-client fd traffic and no protocol parsing.
- **CLOEXEC**: in wrapper mode the broker marks its socket fds
  `FD_CLOEXEC` (`markCloexecAll`, via `/proc/self/fd`) so the child does
  not inherit them. CLOEXEC only affects the exec'd child; the broker
  itself keeps using those fds.

## Dependencies

Only `golang.org/x/sys` (stdlib-adjacent, needed for the raw socket
syscalls). No `x/crypto` — the broker never speaks the agent protocol.
Logging is `log/slog` (stdlib) to stderr, set by `-log-level`. Fatal
errors (invalid flags, unrecoverable setup) are plain text on stderr and
exit 1, via `failf` (which returns the code so the deferred socket
removal runs) — not slog records.
