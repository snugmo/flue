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

12. Doc comment for `markCloexecAll` sits above `sockFlagSet` — **open**.
13. Signal-killed child yields exit 255 instead of 128+signal — **open**.
14. `sendListener` leaks the listener dup on early-error returns —
    **fixed** (dup closed by defer, in the same commit as item 2).
15. Redundant code: second `<-hostGone`, `_ = n`, dead `dir == ""`,
    `cmd.Env = os.Environ()` — **open**.
16. `extract-broker` does not `fsync` before rename — **open**.
17. `dialAgent` reads the global `sockPath` — **open**.
18. `golangci-lint` findings (unchecked `Close`/`Remove` in
    `writableDir`, `extract.go`, `inject.go`; SA9009 in `embed.go`) —
    **open**.
