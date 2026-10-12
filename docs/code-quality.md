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
