# Code quality findings

Findings from a review focused on fd handling, resource leaks, and missing
cleanup. Each item carries a status: **open** or **fixed** (with the commit
subject that fixed it). Update this file when an item is addressed.

## High

1. **Broker skips cleanup on non-zero exit** — **fixed** (`main` now
   calls `run() int`, so the defers run before `os.Exit`).
   `cmd/flue-broker/main.go`: `os.Exit(code)` inside `main` bypasses
   `defer removeQuiet(sock)` / `defer closeQuiet(ctrl)`; `fatalf` after
   `net.Listen` likewise leaves the socket file behind.
2. **One fd wrapped in several `os.File`s (broker)** — **fixed** (the fd
   is wrapped once, the `os.File` closed, and one conn serves handover and
   watcher; `heldControl` removed).
   `heldControl` plus temporary `os.NewFile(fdNum)` objects passed to
   `net.FileConn` (which dups). The temporaries are never closed, so their
   GC finalizers close fd 3 behind `heldControl`.
3. **Host-side fds double-closed / never closed** — **fixed** (end B is
   closed once via its `os.File`; the `os.File`s given to `FileConn` /
   `FileListener` are closed immediately).
   `cmd/flue/main.go`: `childFd` is closed by both `unix.Close(b)` and a
   deferred `closeFile`; the `os.File`s given to `net.FileConn` (control
   end `a`) and `net.FileListener` (received listener) are never closed, so
   closing the conn/listener does not release the socket.
4. **Accept loop dies on any error** — **fixed** (only `net.ErrClosed`
   ends it; other errors warn and retry with backoff). Transient errors
   (`EMFILE`, `ECONNABORTED`) end the loop silently.
5. **Shutdown can hang on a lingering client** — **fixed** (`clientSet`
   closes live client conns once the command exits). `wg.Wait()`
   waits for clients that outlive the container command.

## Medium

6. **`receiveListener` leaks received fds on `MSG_CTRUNC`/parse error** —
   **fixed** (`receivedFDs` collects them; they are closed on every
   rejection path).
7. **`receiveListener` read has no timeout** — **fixed** (30s
   `handoverTimeout`; `TestReceiveListenerTimeout`).
8. **Unbounded per-client resources** (no cap, dial or idle timeout) —
   **fixed** (64 concurrent clients, 5s dial timeout, 10min idle timeout;
   `maxClients`, `dialTimeout`, `idleTimeout`).
9. **Injected memfd is writable and unsealed** — **fixed** (sealed with
   `F_SEAL_{SEAL,SHRINK,GROW,WRITE}` after the write; still executable).
10. **Socket created with umask permissions before chmod 0600** —
    **fixed** (`listenPrivate` binds under umask 0177; the chmod is gone).
11. **Broker does not handle signals; only the direct child is killed** —
    **fixed** for signals (SIGTERM/SIGHUP are forwarded to the child,
    SIGINT swallowed, standalone mode shuts down cleanly; the socket is
    removed in all cases). Killing the whole process group on host
    departure is deliberately *not* done: a separate process group would
    break tty job control for the wrapped shell.

## Low

12. Doc comment for `markCloexecAll` sits above `sockFlagSet` — **fixed**.
13. Signal-killed child yields exit 255 instead of 128+signal — **fixed**
    (`exitStatus`; `TestRunChildSignalExitCode`).
14. `sendListener` leaks the listener dup on early-error returns —
    **fixed** (dup closed by defer, in the same commit as item 2).
15. Redundant code: second `<-hostGone`, `_ = n`, dead `dir == ""`,
    `cmd.Env = os.Environ()` — **fixed**. (`keyFingerprintFromBlob` stays:
    it works on a raw blob without parsing a key.)
16. `broker executable` (then `extract-broker`) does not `fsync`
    before rename — **fixed**.
17. `dialAgent` reads the global `sockPath` — **fixed** (the path travels
    on `agentFilter.sock`; the global remains only as the PreRun→Run
    handoff).
18. `golangci-lint` findings (unchecked `Close`/`Remove` in
    `writableDir`, `extract.go`, `inject.go`; SA9009 in `embed.go`) —
    **fixed**; `golangci-lint run ./...` reports 0 issues. The broker's
    `fatalf` became unused and was folded into `failf`.

## Design review 2026-10-11

Findings from the 2026-10-11 design review
([design-review-2026-10-11.md](design-review-2026-10-11.md)), recorded
here so the two review documents share one tracking file. Numbering
continues from the items above.

19. **Host wrapped an unvalidated listener fd (review item 1)** —
    **fixed** (`🛡️ fix: validate the listener fd the container hands
    over`, 797b945). `receiveListener` wrapped whatever listening socket
    the container sent (including TCP); a compromised container could have
    served the agent over a TCP listener. The fd is now validated
    (`SO_DOMAIN` `AF_UNIX`, `SO_TYPE` `SOCK_STREAM`, `SO_ACCEPTCONN` 1)
    and type-asserted to `*net.UnixListener`.
20. **Certificate fingerprints didn't match `ssh-add -l` (review item
    2)** — **fixed** (`🐛 fix: match certificate fingerprints to
    ssh-add -l`, 8995e22). `List` fingerprinted the whole certificate
    blob, so an `--allow-key` value copied from `ssh-add -l` (the
    underlying key's fingerprint) never matched. `keyFingerprintFromBlob`
    now parses and fingerprints `cert.Key`.
21. **Stale standalone-mode socket examples (review item 3)** —
    **fixed** (`📝 docs: fix stale standalone-mode socket examples`,
    dfcfaa3). The broker generates a per-session path, but the AGENTS.md /
    Containerfile / `exec` help examples set `SSH_AUTH_SOCK` without
    pinning `-sock`. The examples, the `-sock` default and help, and the
    socket-path-coupling rule were all corrected.
22. **Memfd fallback and kernel comments off (review item 4)** —
    **fixed** (`🐛 fix: retry memfd fallback only on EINVAL; correct
    docs`, a7d4912). The fallback retried on any error, so an `EACCES`
    from `vm.memfd_noexec=2` produced a non-executable memfd and a
    confusing in-container failure; it now retries only on `EINVAL` and
    reports the refusal clearly. Corrected `MFD_EXEC` to Linux 6.3 /
    `vm.memfd_noexec` (not 5.17 / MDWE).
23. **Broker showed as `4` in `ps` (review item 5)** — **fixed**
    (`🏷️ chore: broker sets its process name to flue-broker`, 5696911).
    `setProcName` uses `PR_SET_NAME` (a NUL-terminated buffer) so
    `ps` and `/proc/<pid>/comm` show `flue-broker`.
24. **README Security section undersold the tool (review item 6)** —
    **fixed** (`📝 docs: state the actual security guarantee in README`,
    78d5e5c). Replaced the "trusted containers only" framing with the
    real guarantee (the allowlist is the trust boundary); the session-check
    guarantee was added to the same section by task 10.
25. **`agentFilter` served as both policy and per-client filter (review
    smaller 1)** — **fixed** (`♻️ refactor: separate agent policy from
    the per-client filter`, f14e8a6). A `policy{allow, sock}` struct holds
    the session-wide state; `agentFilter` is now always a per-client value
    with a real upstream.
26. **`pickSocketDir` accepted `$XDG_RUNTIME_DIR` without checking
    writability (review smaller 2)** — **fixed** (`🐛 fix: fall through
    when XDG_RUNTIME_DIR is not writable`, 35c3d6c). It now requires
    `writableDir` like the other candidates and falls through on failure.
27. **Comments drifting from the code (review smaller 3)** — **fixed**
    (`🧹 chore: trim comments to the ones that earn their place`,
    3e7e064). Kept the *why* comments; cut *what* narration that restated
    the code.
28. **Session check (review "next step")** — **fixed** (`🔍 feat: learn
    the broker's session id via SCM_CREDENTIALS`, 8d62851; `🔒 feat:
    refuse client connections from a different session`, f8c6203). When
    the broker is a session leader (interactive `flue enter`), each client
    must share the broker's session (`SO_PEERCRED` on the accepted
    connection, session id from `/proc`); an unknown or different session
    is refused, failing closed.

## Review 2026-10-12 (snugmo/flue#2)

Findings from the code review of `6aae5fa` posted as
[snugmo/flue#2](https://github.com/snugmo/flue/issues/2), recorded here
so the review and this file share one tracking location. Numbering
continues from the items above.

29. **Namespace and `MFD_EXEC` comments were inaccurate (review
    smaller)** — **fixed** (`📝 docs: fix the pid-namespace and
    MFD_EXEC comments`, 5b02ae5). The `SCM_CREDENTIALS` / `SO_PEERCRED`
    comments called the pids "root (host) pid namespace" pids; the
    kernel translates them to the **reading** process's namespace (and
    `/proc` shows values in the namespace of its mount) — the same
    result for a host-side flue, but the old wording would mislead a
    flue run from inside a container. `MFD_EXEC` is not "a no-op on
    kernels that predate the sysctl"; it is rejected with `EINVAL`,
    which is why `memfdCreateWithFallback` exists.
30. **The "broker is a session leader" gate silently disabled the
    session check (root cause of the review's items 1–2)** — **fixed**
    (`🔒 feat: gate the session check on the broker's session being
    private`, 96db91d). Current runtimes (podman/crun) create a
    per-exec session led by a short-lived helper that exits before the
    exec'd process runs, so the broker is a *member*, not the leader,
    of its session and the old `session id == broker pid` test never
    fired. It is replaced by a privacy test: at handover, a `/proc`
    scan verifies the broker is the session's only live member;
    membership is inherited by `fork` only, so such a session can later
    hold only the broker and its descendants — with or without a tty.
    (Later refined: that "only live member" test was a proxy that
    misfired when the broker — its own session's leader — had already
    forked its wrapped command before the host's `/proc` walk ran, so a
    required interactive enter aborted on a busy host. It is now decided
    from the session's leader: the broker leads it, or a per-exec helper
    led it and exited — immune to that race.)
31. **Fail closed when the check was expected but can't be enabled
    (review item 1)** — **fixed** (`🛑 fix: fail closed when the session
    check cannot be enabled`, d6e0959). `sessionCheckMode` carries the
    expectation: an injected broker run with a tty (`flue enter` from a
    terminal) makes the check **required** — if it cannot be enabled
    (no credentials, unreadable session, session not private), the
    session is aborted (control channel closed, the broker kills its
    command, flue exits 1) instead of silently serving without the
    second gate. The other modes log their state at `info`.
32. **`--no-inject-broker` lets the check be bypassed (review item
    2)** — **fixed** (same commit, d6e0959). Without injection the
    broker is untrusted image code that could hand over a listener
    from any session it controls, so the check's session reference
    would not be trustworthy: the mode is **off**, the privacy scan is
    skipped, and a `warn` states exactly why.
33. **Refusal warnings can flood the terminal (review item 3)** —
    **fixed** (`📉 fix: rate-limit refusal warnings; add --log-file`,
    766126d). `refusalLimiter` (`cmd/flue/lograte.go`) logs the first
    refusal of a burst in full, summarizes the rest once every 30 s
    (`flue: client refusals`, `more=N`), and flushes the remainder when
    the session ends. The new global `--log-file` sends log output to a
    file (mode `0600`) instead of stderr.
34. **The `SO_PEERPIDFD` comment was wrong (review item 4)** —
    **fixed** (`💓 feat: verify client liveness with SO_PEERPIDFD after
    the /proc read`, f56766e). The comment claimed there is no practical
    way to use a pidfd; in fact the kernel (6.5+) pins the peer's task
    at connect time, so after the `/proc` read `peerPidfdLiveness`
    checks the pidfd (`pidfd_send_signal(pidfd, 0)`: `0`/`EPERM` alive,
    `ESRCH` gone) and refuses a peer that is gone or unverifiable.
    Kernels without the option (`ENOPROTOOPT`) keep the best-effort
    `/proc` read.
35. **`setsid()` in the broker for non-tty mode (review item 5)** —
    **investigated: no change needed**. The privacy gate (item 30)
    already covers the non-tty case: current runtimes give every exec
    its own session with or without `-t`, so a scripted `flue enter`
    or standalone session runs with the check on (advisory) without the
    broker doing anything. A `setsid()` would only matter for runtimes
    whose exec'd process joins the container's init session, where the
    check stays advisory — the same exposure as before the session
    check existed.
36. **Clients that start their own session are refused (review
    smaller)** — **accepted as designed**; confirm-instead-of-deny is
    tracked in [snugmo/flue#1](https://github.com/snugmo/flue/issues/1)
    (deferred until the desktop endpoint exists).

## Follow-up review 2026-10-12 (reply to snugmo/flue#2)

The reply to [snugmo/flue#2](https://github.com/snugmo/flue/issues/2)
confirming the `6aae5fa..7fbf2cb` fixes, with four remaining items and
three smaller points. Numbering continues from the items above.

37. **`flue exec` used `sessionCheckAdvisory` for an untrusted broker
    (review item 1, medium)** — **fixed** (`🚫 fix: use
    sessionCheckOff in the flue exec passthrough`, bafffb5).
    `runPassthrough` runs the broker unmodified from the image — the
    same untrusted-broker case as `--no-inject-broker` — but passed
    `sessionCheckAdvisory`, so flue logged "session check on" when the
    broker's session happened to be private, protecting against
    nothing (a modified broker can hand over its listener from any
    private session it controls). It now passes `sessionCheckOff` and
    logs the standard warning; the mode docs follow (off = no
    injection, advisory = injected broker without a tty).
38. **No escape hatch for the required mode (review item 2,
    low–medium)** — **fixed** (`⚖️ feat: add --session-check to enter
    as an escape hatch for the check mode`, 3fe130d; `🗣 fix: give
    --session-check=off its own warning at handover`, 59cbd37). On a
    runtime that places execs in the container's init session the
    broker's session is never private, so a terminal `flue enter`
    always aborted, and the only workaround, `--no-inject-broker`,
    also gave up the sealed injected broker. `enter
    --session-check=required|advisory|off` now overrides the auto
    choice; an explicit choice is logged at info (a bad value fails
    fast, before the memfd; `TestParseSessionCheckMode`), and explicit
    `off` gets its own warning at handover instead of the
    auto-rule "broker was not injected" one.
39. **The "too many clients" warning was not rate-limited (review item
    3, low)** — **fixed** (`📉 fix: rate-limit the "too many clients"
    warning too`, 8a6752d). The accept-loop cap warning went through
    the session's `refusalLimiter` like the session-check refusals
    (only clients that already passed the check can reach the cap, so
    it was minor, but a container process can still loop against it).
40. **Confirm the private-session behavior on real runtimes (review
    item 4, test)** — **verified** (`🧪 docs: add the private-session
    integration check, verified on podman 5.8.7 / crun 1.28`,
    f380e4d). `flue enter <container> true` logs "session check on"
    both from a terminal (pty: required mode, `podman exec -ti`) and
    piped (advisory, `-i`) — the broker's session was private in both
    (it was the session leader in each case). Verified 2026-10-12 on
    podman 5.8.7 / crun 1.28 / Linux 7.2.9-200.fc44; recorded as item
    8 of the README's manual integration list, with the expected
    behavior on runtimes whose execs share the init session.
41. **Smaller points (review smaller)** — **fixed** (`📝 docs: note
    the pidfd O_CLOEXEC guarantee, the scan race, and fix a header
    wrap`, 303e16f). The `SO_PEERPIDFD` fd is actually created by the
    kernel **with** `O_CLOEXEC` (`__pidfd_prepare` reserves the fd and
    its anon inode with `O_RDWR | O_CLOEXEC`; verified in v6.5 through
    current master, including the pidfs rewrite) and
    `peerPidfdLiveness` closes it immediately anyway — now noted, so
    the desktop-endpoint work keeps that invariant.
    `sessionIsPrivate` documents its theoretical scan race (a member
    forking and exiting mid-scan, with pid wraparound placing the
    child at an already-scanned pid — it needs a second process in
    the session, which is the very thing the scan excludes). The file
    header's "place them before the subcommand" no longer breaks
    oddly.

## Code review 2026-10-21 (11 findings)

42. **Shutdown can hang when the host agent stalls (code-review
    item 1, high)** — **fixed** (`🐛 fix: close upstream agent conns on
    session shutdown`, ddf68ef). `handleClient` wrapped only the client
    side in an `idleConn`; when the host agent accepts but never
    answers, `ServeAgent` blocks on the upstream read with no deadline,
    `closeAll` closed the client but not the upstream, and
    `clients.Wait()` hung on the live handler. The upstream is now
    tracked in the `clientSet` (`track`/`untrack`), `closeAll` closes
    every tracked conn, and `track` returning false covers the race
    where shutdown lands mid-`dialAgent`. Regression:
    `TestHandleClientShutdownUnblocksStalledAgent` (a fake agent that
    holds the connection; the handler must return after `closeAll`).
43. **A handover flue cannot serve must abort the session (code-review
    item 2, high)** — **fixed** (`🛡️ fix: abort the session on a
    handover flue cannot serve`, 903f11b). Every post-read rejection in
    `receiveListener` returned a nil error, so the caller proceeded to
    `cmd.Wait()` with no listener (or an unusable one) and served a
    broken session; the session-check decision switch also ran before
    the fd validation, so required mode could log "session check on"
    and then silently continue. The validation (truncated control
    message, no fd, `getsockopt` failure, not a listening Unix stream
    socket, no `*net.UnixListener` wrap) now happens first, each path
    returns a real error that aborts, and the required-mode abort
    closes the validated listener. Tests:
    `TestReceiveListenerRejectsNoListenerFD`,
    `TestReceiveListenerRejectsTruncatedMessage`, and
    `TestReceiveListenerRejectsWrongFds` now asserting the error.
44. **No warning when the key filter is open (code-review item 3,
    low)** — **fixed** (`⚠️ fix: warn at session start when the key
    filter is open`, 3d8de33). With neither `--allow-key` nor
    `--allow-key-type` set, every key in the host agent was listed and
    signable from the container silently; a session started that way
    now logs a `warn` at handover naming both flags.
45. **`--log-file` followed symlinks (code-review item 4, low)** —
    **fixed** (`🔒 fix: open --log-file with O_NOFOLLOW`, db34937). A
    log path in a shared directory could be pre-planted as a symlink,
    redirecting log output to an arbitrary file with the user's
    privileges; the open now passes `unix.O_NOFOLLOW`, so a planted
    symlink fails the open instead.
46. **The host kept the broker memfd pinned for the whole session
    (code-review item 5, low)** — **fixed** (`📦 fix: release the host
    memfd copy once the child has it`, 0224156). The memfd's
    `os.File` was closed by a `defer` in `runEnter`'s scope, so the
    host held its copy (and the in-memory broker bytes behind it)
    until session end; `runChildWithAgent` now closes every preserved
    file (alongside the control end) as soon as `cmd.Start()` succeeds
    — the child owns its duplicate from then on.
47. **`setProcName`'s unsafe.Pointer handoff lacked a KeepAlive
    (code-review item 6, low)** — **fixed** (`🛡️ fix: keep the prctl
    name buffer alive across the unsafe.Pointer handoff`, 0f7ce2c).
    The `uintptr(unsafe.Pointer(&buf[0]))` conversion is defined only
    within the `Prctl` call expression; `runtime.KeepAlive(buf)` now
    keeps the heap buffer valid while the kernel reads it.
48. **README still described the old member-scan privacy test
    (code-review item 7, docs)** — **fixed** (`📝 docs: describe the
    session-privacy gate from the leader test`, 93ebd15). Two
    passages said a session is private when the broker is "its only
    live member at handover" — the pre-`sessionIsPrivate` scan. Both
    now describe the leader test: the broker leads the session, or the
    leader that led it (a per-exec helper) has already exited.
49. **`enterDashDash`'s comment gave wrong reasoning (code-review
    item 8, docs)** — **fixed** (`📝 docs: correct the enterDashDash
    anchor reasoning`, 3426755). The comment claimed no global flag
    value could precede the subcommand's `enter` word — false (e.g.
    `flue --sock enter enter box -- bash`). The code is correct for a
    different reason: everything computed is suffix-based, so any
    anchor before the first separator yields the same split; the
    comment now says that.
50. **The session check ran on the accept path (code-review item 9,
    medium)** — **fixed** (`⚖️ fix: run the session check off the
    accept path`, fafc651). `checkClientSession` (a `/proc` read plus
    two `getsockopt` calls plus `pidfd_send_signal`) ran synchronously
    in the single accept goroutine, before the client cap, so a
    process connecting in a loop could starve `Accept()` for
    legitimate connects. The check now runs in the per-connection
    handler, after `clients.add` bounds the parallelism;
    `docs/flue.md` notes the placement.
51. **Release workflow actions ran from mutable tags (code-review
    item 10, supply chain)** — **fixed** (`🔏 chore: pin release
    workflow actions to commit SHAs`, 6ab4d6d). All fifteen `uses:` in
    `release.yaml` now pin immutable commit SHAs (the tag kept as a
    trailing comment), so a release is never signed and attested by
    action code that moves; the golangci-lint job pins
    `version: v2.15.0` instead of `latest` (lint was non-reproducible),
    and every checkout sets `persist-credentials: false` (nothing in
    the workflow pushes to the git remote; the repo is public, so the
    release job's tag fetch needs no token) — the GITHUB_TOKEN no
    longer sits in `.git/config` inside a build workspace.
52. **Lint surface covered only errcheck (code-review item 11, low)**
    — **fixed** (`🔍 chore: enable the gosec linter`, 3d1accd). gosec
    now runs alongside errcheck in the lint job. Test files are
    excluded like errcheck (their findings are the scaffolding: fake
    `/proc` trees at real permissions, dials to the test's own
    sockets, the test binary spawning itself). The six production
    findings are audited and suppressed with `#nosec` plus the
    reason: the two `exec.Command` calls that are the tools' entire
    purpose (flue's child, the broker's wrapped command), the
    audited `unsafe.Pointer` prctl name handoff, and a
    `CreateTemp`/`Remove` pair the taint analysis cannot see through.
