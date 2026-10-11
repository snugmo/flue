# Improvement plan — design review 2026-10-11

Plan for addressing the Claude Opus 5.5 review in
[design-review-2026-10-11.md](design-review-2026-10-11.md). Tasks are
sized to fit a single focused change (≈ one commit each); the "next step"
feature (session check) is split into sub-tasks. Each task lists the code
change, the tests to add/update, and the docs that must move with it
(the `cmd/*/main.go` file headers are the authoritative docs, per
AGENTS.md, so they are updated in the same commit as their code).

Suggested implementation order is the task numbering: small, independent
fixes first, then the refactor that the session check builds on, then the
feature itself, and comment hygiene last (so it doesn't get reworked by
the earlier changes).

Status: each task is marked with its fixing commit as it lands.

---

## Task 1 — Host: validate the listener fd the container sends

Review item 1 (hardening; prerequisite for the session check).

- **Where**: `cmd/flue/main.go`, `receiveListener`.
- **Change**: before wrapping the received fd, check it is what we expect:
  - `SO_DOMAIN` == `AF_UNIX`
  - `SO_TYPE` == `SOCK_STREAM`
  - `SO_ACCEPTCONN` == 1
  (all via `unix.GetsockoptInt` on the raw fd, the same pattern the
  broker's `checkControlFD` already uses for the other direction). On any
  mismatch: log the actual values, close the fd, and return no listener
  (the existing `nil, closedChannel()` path). Then type-assert the
  `net.FileListener` result to `*net.UnixListener` and treat a failed
  assertion as a rejection too — this also hands task 10 the concrete
  `*net.UnixListener` type it needs for `SO_PEERCRED`.
- **Tests**: `cmd/flue/serve_test.go` — extend the existing control-pair
  harness (`makeControlPair`) to send (a) a `SOCK_SEQPACKET` socket
  (right domain, wrong type), (b) an un-listened stream socket
  (`SO_ACCEPTCONN` 0), and (c) a TCP listener where the test environment
  allows one; each must be rejected with no listener returned and the fd
  closed.
- **Docs**: `docs/flue.md` Control channel bullet — mention the
  validation. File header of `cmd/flue/main.go`: one clause.
- **Commit**: `🛡️ fix: validate the listener fd the container hands over`
- **Size**: M

## Task 2 — Host: certificate fingerprints must match `ssh-add -l`

Review item 2 (correctness bug).

- **Where**: `cmd/flue/main.go`, `keyFingerprintFromBlob` (used by
  `List`) and, for clarity, the `Sign`/`SignWithFlags` fingerprint path.
- **Change**: parse the blob with `ssh.ParsePublicKey`; if the result is
  an `*ssh.Certificate`, fingerprint `cert.Key` (the underlying plain
  key) instead of hashing the whole certificate blob; otherwise keep the
  current raw-blob hash (identical to `ssh.FingerprintSHA256`). This makes
  `--allow-key` values copied from `ssh-add -l` work for certified keys,
  and keeps `List` consistent with `Sign` (the client sends the plain
  key in the sign request, which is what `Sign` already fingerprints).
- **Tests**: `cmd/flue/filter_test.go` — the fake agent there can carry a
  certificate (build one with `ssh.Certificate` + a CA key from
  `x/crypto/ssh`): assert `List` with an allowlist set to the
  underlying-key fingerprint returns the cert, and that a cert with a
  non-matching base key is filtered out.
- **Docs**: README Flags table `--allow-key` row — one clause:
  fingerprints are compared the way `ssh-add -l` prints them (for a
  certificate, that's the fingerprint of the underlying key).
- **Commit**: `🐛 fix: match certificate fingerprints to ssh-add -l`
- **Size**: S

## Task 3 — Docs: fix the stale standalone-mode socket examples

Review item 3. Since the broker generates a per-session socket path,
standalone mode examples that set `SSH_AUTH_SOCK=/tmp/flue.sock` without
pinning `-sock /tmp/flue.sock` describe a socket that does not exist.

- **Change** (all one coherent edit, all docs/help text — no behavior):
  - `AGENTS.md` "Forward agent" row: add
    `flue-broker -sock /tmp/flue.sock`.
  - `Containerfile` header usage block: same.
  - `cmd/flue/main.go` `execCmd` `Long` text (≈line 208): same. (The
    file header in the same file is already correct — bring the `Long`
    text in line with it.)
  - `cmd/flue-broker/main.go` `-sock` flag: the default is no longer
    `/tmp/flue.sock`; change the default value to `""` and the help text
    to say "unique per-session path by default; set to pin a fixed
    path". Treat an explicitly set-but-empty value as "generate" (or
    reject it with a clear message — decide while in there; simplest is
    generate).
  - `docs/flue-broker.md` Mechanics: "Listens on `-sock` (default
    `/tmp/flue.sock`)" → per-session default.
  - `AGENTS.md` "Adding features → Socket path coupling": the
    `enterSocket` / `TestEnterSocketMatchesBrokerDefault` rule no longer
    exists. Replace with the current invariant: the broker generates a
    unique per-session path (XDG_RUNTIME_DIR → /run/user/\<uid\> → /run
    → /tmp) and exports it as `SSH_AUTH_SOCK`; standalone mode must pin
    `-sock` (the examples in AGENTS.md/Containerfile/help text must stay
    in sync with that rule).
  - While in the broker: `sendListener`'s comment names
    `/tmp/flue.sock` as the path — reword to "the socket path it owns".
- **Tests**: none needed (no behavior change beyond the flag-default
  edge case; `TestPickSocketDir` and the cli tests still pass — run
  them).
- **Commit**: `📝 docs: fix stale standalone-mode socket examples`
- **Size**: S

## Task 4 — Memfd: fix the fallback and the kernel/MDWE comments

Review item 4.

- **Where**: `cmd/flue/inject.go` (`injectBrokerMemfd`), `docs/flue.md`
  (Memfd injection bullet).
- **Change**:
  - Retry `unix.MemfdCreate` without `MFD_EXEC` **only on `EINVAL`**
    (kernel too old to know the flag). Any other error — in particular
    `EACCES` from `vm.memfd_noexec=2` — must fail with a clear message:
    the system refuses executable memfds, so broker injection won't work
    here; disable it (`--no-inject-broker`) or change the sysctl.
  - Correct the comments (code and docs): `MFD_EXEC` arrived in Linux
    6.3 (not 5.17) and gates execution of memfds against the
    `vm.memfd_noexec` sysctl; MDWE (memory-deny-write-execute) is about
    writable+executable mappings and does not affect exec'ing a memfd.
    Drop the MDWE claims from both `inject.go` and `docs/flue.md`.
- **Tests**: `cmd/flue/inject_test.go` — extract the
  create-with-fallback into a small helper that takes the create call as
  a closure (so tests can simulate `EACCES` without a special kernel) and
  assert: `EINVAL` → retry without `MFD_EXEC`; `EACCES` → no retry,
  clear error mentioning `vm.memfd_noexec` / disabling injection.
- **Commit**: `🐛 fix: retry memfd fallback only on EINVAL; correct docs`
- **Size**: S

## Task 5 — Broker: set the process name

Review item 5.

- **Where**: `cmd/flue-broker/main.go`, `run()` (after flag parsing,
  before listen — name is useful even if setup fails).
- **Change**: `unix.Prctl(unix.PR_SET_NAME, ...)` with `"flue-broker"`
  (11 chars, fits the 15+1 limit). `PR_SET_NAME` changes `/proc/<pid>/comm`
  and what `ps` shows; the string needs `unsafe.Pointer` plumbing for the
  C-string arg — a small `setProcName` helper. Failures (non-Linux or
  old kernel) are silently ignored: it's cosmetic.
- **Tests**: `cmd/flue-broker/main_test.go` — call the helper and assert
  `/proc/self/comm` is now `flue-broker` (Linux-only, skip otherwise).
- **Docs**: `docs/flue-broker.md` — one clause in Mechanics (shows as
  `flue-broker` in `ps`/`/proc`).
- **Commit**: `🏷️ chore: broker sets its process name to flue-broker`
- **Size**: S

## Task 6 — README: state the actual security guarantee

Review item 6. No code changes.

- **Where**: README `## Security`.
- **Change**: replace the "Only use this in trusted containers" framing
  with the real guarantee: while a session is open, a process in the
  container can (a) list the allowlisted keys — fingerprint and comment
  are visible — and (b) get signatures from them; it cannot add, remove,
  lock, or unlock keys, nor use any key outside the `--allow-key`
  list, and it never sees a path or fd to the host agent socket, its
  directory, or `$XDG_RUNTIME_DIR`. The trust boundary is the allowlist,
  not the container. Keep it short; the Filter policy table next to it
  already carries the per-operation detail.
- **Follow-up**: when the session check (task 10) lands, extend this
  section with the session-check guarantee in the same section.
- **Commit**: `📝 docs: state the actual security guarantee in README`
- **Size**: S

## Task 7 — Host: split the shared policy out of `agentFilter`

Review "smaller points" item 1 (refactor that makes the session check
cleaner).

- **Where**: `cmd/flue/main.go`.
- **Change**: introduce `type policy struct { allow map[string]struct{};
  sock string }`. `agentFilter` becomes `{ upstream agent.ExtendedAgent;
  pol *policy }` — always a per-client value with a real upstream; the
  nil-upstream shared object disappears. `receiveListener` and
  `handleClient` take the `*policy`; `runChildWithAgent` builds it once
  from the flags. All filter methods go through `f.pol`.
- **Tests**: update `filter_test.go` and `serve_test.go` constructors;
  no behavior change, so the assertions stand — a good proof.
- **Docs**: `docs/flue.md` Agent proxy bullet mentions the filter; note
  the policy struct. (The deny methods in `agentFilter` stay as-is per
  duplication-policy.md.)
- **Commit**: `♻️ refactor: separate agent policy from the per-client filter`
- **Size**: M

## Task 8 — Broker: check that XDG_RUNTIME_DIR is writable

Review "smaller points" item 2.

- **Where**: `cmd/flue-broker/main.go`, `pickSocketDir`.
- **Change**: require `writableDir` (not just `isDir`) for
  `$XDG_RUNTIME_DIR`, like every other candidate; if it's set but not
  writable, fall through to `/run/user/<uid>` → `/run` → `/tmp` instead
  of failing at `listen`.
- **Tests**: `sockpath_test.go` `TestPickSocketDir` — add a case with
  `XDG_RUNTIME_DIR` set to a read-only dir, expecting the fallthrough.
- **Docs**: `docs/flue-broker.md` and the README `flue enter` bullet
  ("XDG_RUNTIME_DIR if set" → "if set and writable").
- **Commit**: `🐛 fix: fall through when XDG_RUNTIME_DIR is not writable`
- **Size**: S

## Task 9 — Session check (new feature), part 1: learn the broker's session

Review "next step" 1–2. Depends on task 7 (state goes on the policy).

- **Where**: `cmd/flue/main.go` (`runChildWithAgent` /
  `receiveListener`); broker: **no changes** — the kernel appends the
  sender's `SCM_CREDENTIALS` to the handover message whenever the
  receiver has `SO_PASSCRED` enabled.
- **Change**:
  - Before starting the child, `unix.SetsockoptInt(a, SOL_SOCKET,
    SO_PASSCRED, 1)` on the host end of the control socketpair.
  - In `receiveListener`, parse the oob (same `ParseSocketControlMessage`
    pattern as `receivedFDs`) for a `SO_PASSCRED` msg with a
    `unix.Ucreds`: the broker's host pid.
  - Read the broker's session id from `/proc/<pid>/stat` (field 6 after
    the `)`-terminated comm — the comm field can contain spaces and
    parens, so parse from the last `)`).
  - Store the session id on the policy (0 = unknown: broker gone, stat
    unreadable, or non-Linux — fail open with a warning, since this
    feature is still landing).
- **Tests**: a socketpair test asserting Ucreds arrive with a written
  message (the existing `makeControlPair` harness); a unit test for the
  `/proc/<pid>/stat` session-id parser (write a fake file with a
  `comm` containing spaces and parens).
- **Docs**: `docs/flue.md` Control channel bullet (credentials piggyback
  on the one handover message); README How-it-works step 3/4, one clause
  each.
- **Commit**: `🔍 feat: learn the broker's session id via SCM_CREDENTIALS`
- **Size**: M

## Task 10 — Session check, part 2: verify each client's session

Review "next step" 3. Depends on tasks 1 and 9.

- **Where**: `cmd/flue/main.go` (accept loop / `handleClient`); the
  `*net.UnixListener` from task 1 gives `*net.UnixConn` accepts.
- **Change**: for each accepted connection, fetch peer credentials via
  `RawConn` + `GetsockoptUcred(SO_PEERCRED)`, read the client's session
  id from `/proc/<pid>/stat` (reuse task 9's parser), and compare with
  the broker's session. This works because the broker is the session
  leader that `podman exec -t` creates, so everything run from the
  terminal shares it. On mismatch (or unknown client pid — client may
  have exited between accept and the check): fail closed — log a warn
  with both session ids and close the connection.
  - Gate: the check is only meaningful when the broker is a session
    leader (wrapper mode with a tty, i.e. `flue enter` with `-ti`).
    Decision to nail down here: default on when the broker session is
    known; in standalone/`exec` mode (broker is not necessarily a
    session leader) log at debug and skip — or add a `--session-check`
    flag if the skip case proves confusing in practice. Choose while
    implementing; document the chosen semantics.
  - Note: `SO_PEERPIDFD` (Linux 6.5+) would give a stable handle
    instead of a pid that can vanish; the pinned x/sys v0.49.0 may not
    export it — if absent, either add the raw constant locally or keep
    `SO_PEERCRED` for now (a vanished client pid fails closed, which is
    the safe direction).
- **Tests**: accept a connection from a known-session test socket,
  assert accepted; forge a mismatch by… (directly forging another
  session's peer creds in-process isn't possible; test the compare
  function and the fail-closed close path with a hand-built conn, and
  cover the happy path end-to-end through the existing harness — both
  sides in the same session, so it must pass).
- **Docs**: README Security (extend task 6's section: the container
  additionally must share the session of the `podman exec` that started
  the broker; a rogue process in another session can't even reach the
  socket), README How-it-works, `docs/flue.md`. Add an item to the
  manual Integration check list (e.g. `setsid` a command that connects
  via `SSH_AUTH_SOCK` and expect it to be refused).
- **Commit**: `🔒 feat: refuse client connections from a different session`
- **Size**: L

## Task 11 — Comment hygiene: shrink the drift surface

Review "smaller points" item 3. Do this **last**, after the functional
changes, so we trim the final text rather than rework it.

- **Where**: `cmd/flue/main.go`, `cmd/flue-broker/main.go`,
  `inject.go`, `extract.go`.
- **Change**: keep the file headers (authoritative, per AGENTS.md) but
  tighten them; in the code, keep *why* comments (invariants, the fd
  aliasing traps, the deliberate duplication) and cut *what* narration
  that merely repeats the code. Target the offenders that have already
  drifted (item 3's examples) as the template.
- **Tests**: none (behavior-neutral; full suite must stay green).
- **Commit**: `🧹 chore: trim comments to the ones that earn their place`
- **Size**: M

## Task 12 — Close-out: record outcomes

- Add the design-review findings to `docs/code-quality.md` (continuing
  its numbering, with **fixed** status and commit subjects) so the two
  review documents share one tracking file.
- Mark each task in this file **fixed (commit)**.
- `docs/design-review-2026-10-11.md` stays as a dated artifact, unedited.
- **Commit**: `📝 docs: record design-review outcomes in code-quality.md`
- **Size**: S

---

## Verification

Every task: the AGENTS.md Test command
(`go build -o cmd/flue/flue-broker.bin ./cmd/flue-broker && go test
./...`) plus `golangci-lint run ./...` (currently zero findings — keep it
that way). Task 10 additionally gets a real end-to-end pass from the
README Integration check list, including the new `setsid` refusal case.

## Not in scope (from the review, deliberately)

- The review's praise items (umask, cloexec, sealing, caps, timeouts,
  exit codes, goleak) — no changes requested.
- `SO_PEERPIDFD` as the primary mechanism (see task 10 note) — only if
  the pinned x/sys can't be extended cheaply; the pid-based path is
  fail-closed by design.
- Collapsing the deny methods or de-duplicating `parseLevel`/`closeQuiet`
  — explicitly out by duplication-policy.md.
