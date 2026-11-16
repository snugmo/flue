# Code quality findings

The review findings log: fd handling, resource leaks, missing cleanup,
and agent-bridge security, from the initial review through the
2026-11-02 security review. **All items are fixed except item 56**
(open, deferred by the user). This file is the index; the detailed
rationale for each fix lives in the fixing commit and in the current
docs (the `cmd/*/main.go` file headers, `docs/flue.md`). Add new
items here, each with a status — **open** or **fixed** (with the
fixing commit) — and update the status when an item is addressed.

## Initial review (fd handling, leaks, cleanup)

1. **Broker skips cleanup on non-zero exit** — **fixed**: the
   broker's `main` calls `run() int`, so the defers run before
   `os.Exit`.
2. **One fd wrapped in several `os.File`s (broker)** — **fixed**: the
   fd is wrapped once; one conn serves handover and watcher.
3. **Host-side fds double-closed / never closed** — **fixed**: end B
   is closed once via its `os.File`; the `os.File`s given to
   `net.FileConn` / `net.FileListener` are closed immediately.
4. **Accept loop dies on any error** — **fixed**: only `net.ErrClosed`
   ends it; other errors warn and retry with backoff.
5. **Shutdown can hang on a lingering client** — **fixed**: the
   `clientSet` closes live client conns once the command exits.
6. **`receiveListener` leaks received fds on `MSG_CTRUNC` / parse
   error** — **fixed**: they are closed on every rejection path.
7. **`receiveListener` read has no timeout** — **fixed**: 30 s
   `handoverTimeout`.
8. **Unbounded per-client resources** — **fixed**: 64 concurrent
   clients, 5 s dial timeout, 10 min idle timeout.
9. **Injected memfd is writable and unsealed** — **fixed**: sealed
   with `F_SEAL_{SEAL,SHRINK,GROW,WRITE}` after the write; still
   executable.
10. **Socket created with umask permissions before chmod 0600** —
    **fixed**: `listenPrivate` binds under umask 0177; the chmod is
    gone.
11. **Broker does not handle signals** — **fixed**: SIGTERM/SIGHUP
    forwarded to the child, SIGINT swallowed, standalone mode shuts
    down cleanly, the socket is removed in all cases. Killing the
    whole process group on host departure is deliberately *not* done —
    a separate process group would break tty job control for the
    wrapped shell.
12. **Doc comment for `markCloexecAll` sits above `sockFlagSet`** —
    **fixed**.
13. **Signal-killed child yields exit 255 instead of 128+signal** —
    **fixed** (`exitStatus`).
14. **`sendListener` leaks the listener dup on early-error returns** —
    **fixed** (the dup is closed by defer).
15. **Redundant code** (second `<-hostGone`, `_ = n`, dead `dir == ""`,
    `cmd.Env = os.Environ()`) — **fixed** (`keyFingerprintFromBlob`
    stays: it works on a raw blob without parsing a key).
16. **`broker executable` does not `fsync` before rename** —
    **fixed**.
17. **`dialAgent` reads the global `sockPath`** — **fixed**: the path
    travels on `agentFilter.sock`; the global remains only as the
    PreRun→Run handoff.
18. **`golangci-lint` findings** — **fixed** (0 issues; the broker's
    then-unused `fatalf` was folded into `failf`).

## Design review 2026-10-11 (items 19–28)

The review is [design-review-2026-10-11.md](design-review-2026-10-11.md)
(Claude Opus 5.5); the fixing plan was
[improvement-plan-2026-10-11.md](improvement-plan-2026-10-11.md).

19. **Host wrapped an unvalidated listener fd** — **fixed** (797b945):
    the handover fd is validated (`AF_UNIX` / `SOCK_STREAM` /
    `SO_ACCEPTCONN=1`) and type-asserted to `*net.UnixListener` — a
    host-networked container cannot serve the agent over a TCP
    listener.
20. **Certificate fingerprints didn't match `ssh-add -l`** — **fixed**
    (8995e22): `List` fingerprints `cert.Key`, not the whole
    certificate blob.
21. **Stale standalone-mode socket examples** — **fixed** (dfcfaa3):
    the examples now pin `-sock`; the `-sock` default and help say
    unique per-session path.
22. **Memfd fallback and kernel comments off** — **fixed** (a7d4912):
    the fallback retries only on `EINVAL` (an `EACCES` from
    `vm.memfd_noexec=2` is a hard error, not a retry); `MFD_EXEC`
    corrected to Linux 6.3 / `vm.memfd_noexec`.
23. **Broker showed as `4` in `ps`** — **fixed** (5696911):
    `setProcName` uses `PR_SET_NAME`.
24. **README Security section undersold the tool** — **fixed**
    (78d5e5c): the real guarantee (the allowlists are the trust
    boundary) is stated.
25. **`agentFilter` served as both policy and per-client filter** —
    **fixed** (f14e8a6): a `policy` struct holds the session-wide
    state; the filter is always a per-client value with a real
    upstream.
26. **`pickSocketDir` accepted `$XDG_RUNTIME_DIR` without checking
    writability** — **fixed** (35c3d6c).
27. **Comments drifting from the code** — **fixed** (3e7e064): kept
    the *why* comments, cut *what* narration that restated the code.
28. **Session check** — **fixed** (8d62851, f8c6203): the broker's
    session id is learned via `SCM_CREDENTIALS`; clients that do not
    share it are refused, failing closed.

## Review 2026-10-12, snugmo/flue#2 (items 29–36)

29. **Namespace and `MFD_EXEC` comments were inaccurate** — **fixed**
    (5b02ae5): the pids are in the *reading* process's namespace (the
    kernel translates them); `MFD_EXEC` on old kernels is rejected
    with `EINVAL`, not a no-op.
30. **The "broker is a session leader" gate silently disabled the
    session check** — **fixed** (96db91d, later refined): the leader
    test replaces the scan — a session is private when the broker
    leads it, or the per-exec helper that led it has already exited —
    which is immune to the race the first version (an "only live
    member" scan) had.
31. **Fail closed when the check is expected but cannot be enabled** —
    **fixed** (d6e0959): `sessionCheckMode` carries the expectation
    (required / off / advisory); required mode aborts the session
    rather than serve without the second gate.
32. **`--no-inject-broker` lets the check be bypassed** — **fixed**
    (d6e0959): an untrusted broker → mode **off**, the privacy scan is
    skipped, and a warning states why.
33. **Refusal warnings can flood the terminal** — **fixed**
    (766126d): `refusalLimiter` rate-limits; new global `--log-file`.
34. **The `SO_PEERPIDFD` comment was wrong** — **fixed** (f56766e):
    the kernel (6.5+) pins the peer's task at connect time, so the
    pidfd liveness check after the `/proc` read is sound.
35. **`setsid()` in the broker for non-tty mode** — **investigated:
    no change needed** (the privacy gate already covers non-tty).
36. **Clients that start their own session are refused** — **accepted
    as designed**; confirm-instead-of-deny is
    [snugmo/flue#1](https://github.com/snugmo/flue/issues/1) (deferred
    until the desktop endpoint exists).

## Follow-up review 2026-10-12 (items 37–41)

37. **The `flue exec` passthrough (now `flue debug`, ac28d36) used an
    advisory check for an untrusted broker** — **fixed** (bafffb5): it
    now uses `sessionCheckOff` with the standard warning.
38. **No escape hatch for the required mode** — **fixed** (3fe130d,
    59cbd37): `enter --session-check=required|advisory|off` overrides
    the auto choice; explicit `off` gets its own handover warning.
39. **The "too many clients" warning was not rate-limited** —
    **fixed** (8a6752d).
40. **Private-session behavior on real runtimes** — **verified**
    (f380e4d) on podman 5.8.7 / crun 1.28 / Linux 7.2 (2026-10-12):
    the check is on both from a terminal (required) and piped
    (advisory). Now item 10 of the README's manual integration list.
41. **Smaller points** — **fixed** (303e16f): the `SO_PEERPIDFD` fd is
    created by the kernel with `O_CLOEXEC`; the theoretical scan race
    in `sessionIsPrivate` is documented.

## Code review 2026-10-21 (items 42–52)

42. **Shutdown can hang when the host agent stalls** — **fixed**
    (ddf68ef): the per-client upstream conn is tracked in the
    `clientSet` and closed on shutdown, so a handler blocked reading
    an unresponsive agent unblocks.
43. **A handover flue cannot serve must abort the session** —
    **fixed** (903f11b): the fd validation runs first, and every
    rejection path returns an error that aborts.
44. **No warning when the key filter is open** — **fixed** (3d8de33):
    a session with no allowlist logs a `warn` at handover (the later
    fail-closed gate, item 54, supersedes it for sessions that set
    neither list).
45. **`--log-file` followed symlinks** — **fixed** (db34937): the open
    passes `unix.O_NOFOLLOW`, so a planted symlink fails the open.
46. **The host kept the broker memfd pinned for the whole session** —
    **fixed** (0224156): the preserved files are closed as soon as
    `cmd.Start()` succeeds — the child owns its duplicates from then
    on.
47. **`setProcName`'s unsafe.Pointer handoff lacked a KeepAlive** —
    **fixed** (0f7ce2c).
48. **README still described the old member-scan privacy test** —
    **fixed** (93ebd15): both passages now describe the leader test.
49. **`enterDashDash`'s comment gave wrong reasoning** — **fixed**
    (3426755): the split is suffix-based, so any anchor before the
    first separator yields the same result.
50. **The session check ran on the accept path** — **fixed**
    (fafc651): it now runs in the per-connection handler, after the
    client cap bounds parallelism — a loop-connecting process cannot
    starve `Accept()`.
51. **Release workflow actions ran from mutable tags** — **fixed**
    (6ab4d6d): all `uses:` pin commit SHAs, the lint job pins its
    version, and every checkout sets `persist-credentials: false`.
52. **Lint surface covered only errcheck** — **fixed** (3d1accd):
    gosec runs alongside; the production findings are audited and
    suppressed with `#nosec` + reason, and test files are excluded.

## Security review 2026-11-02 (items 53–56)

53. **`session-bind@openssh.com` was forwarded with no way to stop it
    (CVE-2026-73281)** — **fixed** (530a3b3): `--no-session-bind`
    stops the forwarding; the binding itself is cryptographically
    enforced, and the CVE's PKCS#11-add half is unreachable (the
    smartcard opcodes are not implemented).
54. **The key filter was open by default** — **fixed** (530a3b3):
    `runChildWithAgent` now fails closed — a session with no
    allowlist is refused before the child is spawned unless the
    explicit `--allow-all` opts in (with a session-start warning).
55. **The accepted-by-design residual risks were undocumented** —
    **fixed** (the commit documenting them): the four limitations
    (no session check in the passthrough / `--no-inject-broker`;
    `--allow-all` exposure; the `Signers` denial; smartcard keys
    invisible) are in the README's Security section and
    `docs/flue.md`.
56. **`.idea/` is not in `.gitignore`** — **open** (deferred by the
    user): the IDE's project files are untracked but not ignored (the
    old rule was a comment, removed in 955a02c), so they can be
    committed by accident.
