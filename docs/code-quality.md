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
16. `extract-broker` does not `fsync` before rename — **fixed**.
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
