<img src="assets/flue-agent.webp" alt="A cheerful cartoon key in a black beanie and night-vision goggles, carrying a rope and a sack, climbs into a smiling brick chimney on a snowy rooftop under a full moon." width="100%" />

# flue

A narrow channel from your host's SSH agent into a container, with host-side
key filtering.

Two small Go binaries:

- **`flue`** (host-side) creates a Unix socketpair, passes one end into the
  container, receives the broker's listening fd, and proxies each client
  connection to the real `$SSH_AUTH_SOCK` with a key-allowlist filter. Its
  **`flue enter`** subcommand is the daily driver: it runs the `podman exec`
  for you and hands your command to the broker in wrapper mode. `flue` also
  **embeds** the `flue-broker` built at the same time (`go:embed`) and, by
  default, hands it to the container in memory.
- **`flue-broker`** (container-side) listens on a Unix socket inside the
  container and hands that listening socket's file descriptor to the host
  once over a socketpair control channel (SCM_RIGHTS). In wrapper mode it
  launches your command with `SSH_AUTH_SOCK` pointed at its socket; in
  standalone mode it just watches for the host to go away. No protocol
  parsing.

The usual flow, in one line:

```sh
flue enter <container>     # or just flue enter — cote is the default
```

…and you are in a shell where `ssh-add -l`, `git push`, and friends use the
host's agent — with only the keys you allowlisted visible.

Both binaries are built from source (`cmd/flue-broker/` and `cmd/flue/`)
and are fully static (no runtime deps). The published image
(`ghcr.io/snugmo/flue`) is just the `flue` binary at `/flue`, with the
`flue-broker` built at the same time embedded in it — recoverable via
`flue broker executable` — plus the broker's cosign-signed sigstore
bundle (`flue broker sigstore-bundle`), so a recovered broker is
verifiable offline with `cosign verify-blob --bundle` (see Release
image).

> **Field-verified end to end.** The full path has been exercised for real:
> laptop `ssh-agent` → host workstation → a snugmo container, where
> `git push` over SSH ran with the askpass prompt surfacing on the laptop's
> Wayland desktop. Signing round-trips (not just `ssh-add -l`) work across
> podman, with only the allowlisted key visible in the container.

## Usage

### Prerequisites

1. An SSH agent is running on the host where `flue` runs
   (`SSH_AUTH_SOCK` is set, `ssh-add -l` lists keys).
2. The container is running.

That is all for the usual flow: by default `flue enter` injects the broker
into the container in memory, so the container needs neither a writable
filesystem nor a `flue-broker` in its image. (If you disable injection —
see [Flags](#flags) — the image must provide `flue-broker`; see
[Getting a broker into a container](#getting-a-broker-into-a-container).)

### Getting in: `flue enter`

`flue enter` runs the `podman exec` for you: it preserves the control
socket as fd 3, hands your command to `flue-broker` in wrapper mode,
targets the `cote` container when you give none, and allocates a tty
when your stdin and stdout are both terminals (a pty is only useful
when both ends of the conversation are terminals). When you give no
command, the broker resolves
a shell inside the container:

```sh
flue enter                                  # a shell in cote
flue enter <container>                      # a shell in the container
flue enter <container> bash                 # bash
flue enter <container> git ls-remote git@github.com:you/repo.git
flue enter <container> -- bash -l           # command with its own flags
flue enter -- bash -l                       # bash -l in the default container
```

A `--` with no argument before it makes everything after it the command run
in the default container — `flue enter -- bash` does *not* mean a container
named `bash`. With a container before it, the `--` only separates the
target command's own flags.

The default shell is resolved by the broker inside the container — so
against the container's filesystem and environment, not the host's:
`$SHELL` when set and usable (and listed in `/etc/shells` when that file
has entries), else `/bin/bash`, `/bin/sh`, then `bash` or `sh` in
`$PATH`. A set-but-unusable `$SHELL` is logged at warn with the value
and the reason, so the fallback is never silent. Set `$FLUE_ENTER_CMD`
to run something else by default (the
value is split on whitespace and applied on the host; a command given on
the command line always wins):

```sh
FLUE_ENTER_CMD=bash flue enter           # bash in cote
FLUE_ENTER_CMD="bash -l" flue enter dev  # a login bash in dev
```

Inside the session, the agent just works: the broker listens on a socket
that is unique per session (several `flue enter`s can run at once without
colliding) and exports that path to your command as `SSH_AUTH_SOCK`, so
`ssh-add -l`, `git push`, and the like reach the host agent through the
filter. When your command exits, `flue enter` returns its exit code.

The broker is **not taken from the image by default**: `flue` writes the
embedded `flue-broker` into an in-memory `memfd` (fd 4, `--preserve-fds 2`)
and the container execs it via `/proc/self/fd/4`, so the broker never
touches the container's filesystem — it works in a container with no
writable filesystem and no `flue-broker` in the image at all (the
injection mechanics are in
[docs/embedded-broker.md](docs/embedded-broker.md)). Disable
injection to use the image's `flue-broker` instead:

```sh
flue enter --no-inject-broker <container>       # or --inject-broker=false
FLUE_INJECT_BROKER=0 flue enter <container>     # via the environment
```

Global flags (`--sock`, `--allow-key`, `--allow-key-file`,
`--allow-key-type`, `--allow-all`,
`--no-session-bind`, `--log-level`, `--log-file`) go
before the
subcommand: `flue --allow-key SHA256:… enter <container>`. (The subcommand
`--` separator above is for your *target command's* flags.)

### Flags

**flue** (host-side; persistent flags, placed before the subcommand):

How the allowlist flags combine: a key must pass **every** list that is set
(an unset list is unrestricted) — `--allow-key K --allow-key-type ssh-ed25519`
admits K only if K is an ed25519 key. `--allow-key-file` keys union into the
`--allow-key` list (one key list, which still intersects `--allow-key-type`).
A certificate matches by its underlying key's fingerprint and by both its
certificate and underlying key types. With no key or type list set,
`--allow-all` is required to start (the filter is fail closed).

| Flag          | Default                        | Description                                                                                                                                                                                              |
| ------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--sock`      | `$SSH_AUTH_SOCK`               | Agent socket path to proxy to                                                                                                                                                                            |
| `--allow-key` | (none)                         | Repeatable. Only keys with this SHA256 fingerprint are listed and can sign. Fingerprints are compared the way `ssh-add -l` prints them — for a certificate, that is the fingerprint of its underlying key |
| `--allow-key-file` | (none)                     | Path to an `authorized_keys`-format file (one public key per line; blank lines and `#` comments skipped; an OpenSSH option prefix on a line is allowed; CRLF fine). Parsed fail closed: an unreadable file, a line that does not parse as a key, or a file with no keys is refused before the session starts |
| `--allow-key-type` | (none)                     | Repeatable. Only keys of this type (the wire type name, e.g. `ssh-ed25519`, `ecdsa-sha2-nistp256`, `rsa`) are listed and can sign; secure-enclave keys have their own `sk-` type names |
| `--allow-all` | `false`                        | Explicitly allow every key in the host agent. Required to start a session with no key or type list set. It is the opt-in to the open filter, not an override: with a non-empty allowlist it is inert — the allowlist still applies — and it logs a session-start warning in both cases |
| `--no-session-bind` | `false`                  | Do not forward the `session-bind@openssh.com` agent extension, disabling OpenSSH destination-restricted keys. Provided for host agents affected by CVE-2026-73281 (OpenSSH 8.9–10.4) |
| `--log-level` | `$FLUE_LOG_LEVEL`, else `info` | Log level: `debug`, `info`, `warn`, or `error`. `debug` logs every agent request (operation, key type, fingerprint, allowed/denied — and, when an allowlist denies a key, which list did, as a `reason` field). When the flag is unset, `$FLUE_LOG_LEVEL` is read; a set flag wins            |
| `--log-file`  | `stderr`                       | Append log output to this file (created if needed, mode `0600`) instead of stderr — keeps session-check refusal warnings off the user's terminal                                                                                              |
| `--version`   | -                              | Print the build version and exit. The version is stamped at build time (`-ldflags "-X main.version=<v>"`); a plain `go build` reports `dev`. The `flue version` subcommand prints the same line |

**enter** (enter-scoped flag and arguments):

| Flag              | Default                                     | Description                                                                                                                                                                                                                                                                              |
| ----------------- | ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| *(arguments)*     | `cote`, a shell resolved in the container | Positional: the container to exec into (default `cote`), then the command to run inside it (default `$FLUE_ENTER_CMD` when set and non-blank — split on whitespace — else a shell the broker resolves in the container: `$SHELL`, `/bin/bash`, `/bin/sh`, then `bash`/`sh` in `$PATH`) |
| `--inject-broker` | `true`                                      | Run the broker from an in-memory `memfd` (exec'd via `/proc/self/fd/4`) instead of the image's `flue-broker`; no writable filesystem needed. Disable with `--no-inject-broker`, `--inject-broker=false`, or `FLUE_INJECT_BROKER=0` (a set flag wins over the variable)                   |
| `--session-check` | auto                                        | Override the session check mode: `required` (abort the session if the check cannot be enabled), `advisory`, or `off`. Default is auto: required for an injected broker with a tty, off without injection, advisory otherwise                                                             |

**flue-broker** (container-side):

| Flag             | Default                                   | Description                                                                                                                                                                                                              |
| ------------------ | ------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-sock`          | unique per session (`flue-<8hex>.sock`)   | Unix socket path to listen on. When unset (or empty), a unique path is generated in the first writable of `$XDG_RUNTIME_DIR`, `/run/user/<uid>`, `/run`, `/tmp`; set it (e.g. to `/tmp/flue.sock`) to pin a fixed path   |
| `-fd`            | `3`                                       | File descriptor number of the preserved control socket                                                                                                                                                                   |
| `-default-shell` | `off`                                     | Run a shell resolved in the container instead of a positional command: `$SHELL` when set and usable, else `/bin/bash`, `/bin/sh`, then `bash`/`sh` in `$PATH`; what `flue enter` passes with no command                  |
| `-log-level`     | `info`                                    | Log level: `debug`, `info`, `warn`, or `error`                                                                                                                                                                           |
| `-version`       | -                                         | Print the build version and exit (no agent socket or control fd needed)                                                                                                                                                  |

After the flags, an optional command starts wrapper mode: `flue-broker
[command] [args…]` runs that command as a child with `SSH_AUTH_SOCK` set to
the broker's socket. No command = standalone mode, which blocks until the
host goes away.

These are [cobra](https://github.com/spf13/cobra) persistent flags, so they
accept the `--flag value` or `--flag=value` forms and go before the
subcommand (`flue --sock /path debug -- …`). A subcommand's arguments follow
the subcommand name; a `--` separates them from the target command so the
target's flags pass through verbatim.

### Manual: `flue debug` (the low-level escape hatch)

If `flue enter`'s built-in `podman exec` fails, or you prefer to assemble
the container command yourself (a non-podman runtime, extra flags),
`flue debug` runs any command that receives the control socketpair (over
fd 3) — you write the `podman exec`, flue does the rest. Wrapper mode is
the normal shape — the broker sets `SSH_AUTH_SOCK` for its child, so no
`-e` is needed:

```sh
flue --allow-all debug -- podman exec --preserve-fds 1 <container> flue-broker bash
```

In standalone mode (no command after `flue-broker`) nothing sets
`SSH_AUTH_SOCK` for you, so pin `-sock` to a fixed path and export the same
one:

```sh
flue debug -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> \
  flue-broker -sock /tmp/flue.sock
```

`flue` dials `$SSH_AUTH_SOCK` (or `--sock`) once per client connection, so
repeated `ssh` / `ssh-add` / `git` invocations all work. (The older
`sh -c 'flue-broker & exec bash'` form still works but is superseded by
wrapper mode.)

### Getting a broker into a container

Only relevant when you disable injection (`--no-inject-broker`): the
container then needs the `flue-broker` binary — either from an image that
provides one, or by installing a copy of the embedded one:

```sh
flue broker executable /tmp/flue-broker
podman cp /tmp/flue-broker <container>:/usr/local/bin/flue-broker
```

`flue` embeds the `flue-broker` built at the same time (`go:embed`), so
`broker executable` writes out a byte-identical copy (mode 0755) —
built for the same architecture as your `flue`. Unlike `debug` and
`enter`, the `broker` subcommands need no agent socket and take no
flags.

The usual flow never gets here — `flue enter` injects the embedded
broker into the container in memory by default, so no broker file is
ever written or installed. For how that injection works (the memfd, its
seals, the fd-4 handoff, the kernel fallbacks) and for the other two
`broker` sub-subcommands (`sha256`, `sigstore-bundle`) — including
offline verification of a recovered broker with
`cosign verify-blob --bundle` — see
[docs/embedded-broker.md](docs/embedded-broker.md).

### Version

Both binaries carry their build version: `flue version` (or
`flue --version`) and `flue-broker -version` print it and exit. The
version is a package variable stamped at link time with
`-ldflags "-X main.version=<version>"` — the Containerfile's build
stage stamps it from the `VERSION` build arg (the release workflow
passes the release tag), and a plain `go build` leaves it at `dev`.

## Build

Local image builds are a plain `buildah` invocation — rootless, no
daemon required:

```bash
buildah build --isolation chroot --layers --format docker \
  --build-arg VERSION=0.4.0 \
  --build-arg REVISION=$(git rev-parse HEAD) \
  --build-arg CREATED=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t ghcr.io/snugmo/flue:0.4.0 \
  -f Containerfile .
buildah push ghcr.io/snugmo/flue:0.4.0
```

A plain build produces the host's architecture; for another, pass
`--platform linux/<arch>` (never `--build-arg TARGET*` overrides —
the Containerfile's platform builtins are deliberately default-less
so each stage inherits its own platform; the stages cross-compile
natively, no QEMU). Details: the [Containerfile
header](Containerfile) and [docs/build.md](docs/build.md).

### Release image

Pushing a `v*` tag publishes `ghcr.io/snugmo/flue:<tag>` (and `:latest`)
to GHCR, built by the
[release workflow](.github/workflows/release.yaml) from the same
`Containerfile`: one buildx build per architecture, each from its own
build-context copy carrying that architecture's cosign-signed broker
bundle (a single multi-arch build shares one context, so per-platform
bundles cannot ride one build), the per-architecture images then
assembled into the manifest list with `docker buildx imagetools
create`. What is signed and attested:

- the **manifest list** — a Sigstore keyless cosign signature and a
  SLSA build-provenance attestation (one per architecture), both keyed
  to the list digest, so they cover all of its tags. The signature's
  certificate identity is the SAN URI of the release workflow file at
  the pushed ref — *not* the account that pushed the tag (that is how
  fulcio identifies a GitHub Actions run; the identity embeds the ref,
  so it differs per tag).
- **each architecture's `flue` binary** (a release asset), signed and
  shipping its self-contained `.sigstore.json` bundle next to it. The
  binaries are extracted from the pushed, signed, attested image, so
  they are byte-identical to the image's.
- **the two `flue-broker` binaries**, exported from the Containerfile's
  `flue-broker` stage — the exact files the `flue` build stage
  embedded — attested and signed; each platform's bundle is embedded
  in its `flue`. A broker recovered from any release `flue`
  (`flue broker executable` + `flue broker sigstore-bundle`) verifies
  offline with `cosign verify-blob --bundle`, or against the GHCR
  attestations with `gh attestation verify`.

Verify from any machine:

```sh
tag=1.2.3   # the release tag, without the leading v
identity="https://github.com/snugmo/flue/.github/workflows/release.yaml@refs/tags/v${tag}"

# 1. Signature: this workflow file at this tag, under the GHA OIDC issuer
cosign verify ghcr.io/snugmo/flue:$tag \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"

# 2. Provenance: built by snugmo/flue's release workflow
gh attestation verify ghcr.io/snugmo/flue:$tag --source snugmo/flue

# 3. Binaries: each asset ships a self-contained sigstore bundle
#    (same identity as the image signature: same workflow file, same ref)
cosign verify-blob "flue_${tag}_linux_amd64" \
  --bundle "flue_${tag}_linux_amd64.sigstore.json" \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"

# 4. The broker embedded in a release flue: recover the signed pair
#    from the binary itself (broker executable + broker
#    sigstore-bundle) and verify it offline — the bundle's certificate
#    carries the same identity as the image signature
flue_${tag}_linux_amd64 broker executable /tmp/flue-broker
flue_${tag}_linux_amd64 broker sigstore-bundle /tmp/flue-broker.sigstore.json
cosign verify-blob /tmp/flue-broker \
  --bundle /tmp/flue-broker.sigstore.json \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

## How it works

1. `flue` creates a `SOCK_SEQPACKET` Unix socketpair and passes one end
   into the container as fd 3 via `podman exec --preserve-fds` (2 when the
   broker memfd is also passed as fd 4); it keeps the other end as the
   control channel.
2. `flue-broker` listens on a socket — a unique per-session path unless
   `-sock` pins one — and sends that listening socket's fd to the host
   over the control socket (SCM_RIGHTS) exactly once. Because the host
   end has `SO_PASSCRED` enabled, the same message carries the broker's
   credentials (pid, and with it its session id). The broker then either
   (standalone) blocks watching for the host to go away, or (wrapper
   mode) launches the wrapped command with `SSH_AUTH_SOCK` pointed at
   its socket and waits for it.
3. `flue` receives the listener fd and `accept()`s client connections
   directly; for each one it dials a fresh connection to `$SSH_AUTH_SOCK`
   and runs `agent.ServeAgent(filter, clientConn)` — the filter enforces
   the key allowlist (table below) on the upstream agent client.
4. The **session check** (a second gate) refuses clients that do not
   share the broker's session, when that session is *private* — the
   broker leads it, or the leader that led it has already exited (what
   an exec that created its own session looks like, with or without a
   tty). Its mode: **required** for an interactive `flue enter`
   (injected broker with a tty — if it cannot be enabled the session is
   aborted, not served), **off** with `--no-inject-broker` or in the
   `flue debug` passthrough (the broker is then untrusted code; a
   warning is logged), **advisory** otherwise (state logged at `info`).
   `enter --session-check=required|advisory|off` overrides the choice —
   the escape hatch for runtimes whose execs share a session. Mechanics
   (`SO_PEERCRED`, `/proc`, the `SO_PEERPIDFD` liveness check, rate
   limiting): [docs/flue.md](docs/flue.md#session-check).
5. Lifetime is driven by the process tree, not the control channel. In
   wrapper mode the broker exits when the wrapped command does, the
   `podman exec` returns, and `flue` — blocked on that child — tears
   down the listener and exits with the command's exit status. In
   standalone mode `flue` runs until it is signalled.

### Filter policy

| Operation       | Behavior                                              |
| --------------- | ----------------------------------------------------- |
| `List`          | Forwarded; only keys passing the allowlists (see [Flags](#flags)) are returned. All keys only in the `--allow-all` open filter |
| `Sign`          | Forwarded only for keys passing the same lists        |
| `SignWithFlags` | Forwarded only for keys passing the same lists (needed for RSA rsa-sha2-256/512) |
| `Add`, `Remove`, `RemoveAll`, `Lock`, `Unlock` | Denied |
| `Signers`       | Denied — a Go-only `Agent` interface method with no wire opcode: `ServeAgent` answers the identities request (opcode 11/12) from `List()`, which the filter already keys, so the denial breaks nothing (it keeps the filter's surface minimal). Go clients using `agent.Signers()` get the filtered key list; OpenSSH clients use the filtered `List` for `ssh-add -L` |
| `Extension`     | Only `session-bind@openssh.com` is forwarded (unless `--no-session-bind` is set, in which case no extension is forwarded); all others return `ErrExtensionUnsupported` |

## Security

While a session is open, a process in the container can (a) list the
allowlisted keys — fingerprint and comment are visible — and (b) get
signatures from them. It cannot add, remove, lock, or unlock keys, nor
use any key outside the allowlists, and it never sees a path or fd to
the host agent socket, its directory, or `$XDG_RUNTIME_DIR`. **The trust
boundary is the allowlists, not the container.** The filter is fail
closed by default (a session with no allowlist is refused before it
starts; the safe configuration is one the user has to choose, not one
the tool guesses), and `--allow-all` — the widest exposure, every key
in the host agent listed to the container — is the containment
mechanism's opt-in, so a session that only needs one key should name it.

`session-bind@openssh.com` (the one forwarded extension) is the
mechanism for OpenSSH destination-restricted keys; the binding is
cryptographically enforced (an unforgeable hostkey plus KEX signature),
so a container cannot claim a destination it did not in fact open.
Caveat: OpenSSH 8.9–10.4 carries a lock-and-`session-bind` interaction
(CVE-2026-73281, fixed in 10.5) in which a locked agent can let a
remote (container-mediated) session use destination-restricted keys.
`flue` cannot patch that agent-side bug: run the host agent on OpenSSH
≥ 10.5, or pass `--no-session-bind` (which disables
destination-restricted keys). The CVE's other half — adding PKCS#11
tokens remotely — is unreachable through `flue`, because its agent proxy
does not implement the smartcard opcodes. For the same reason,
smartcard-backed keys (PKCS#11 tokens, secure elements) do not work
through `flue` at all: a token's keys never appear in the filtered list
— if all of your agent's keys live on a token, the session will look
empty.

The session check (step 4 above) adds the second gate: when the broker's
session is private, a rogue process that starts its own session (e.g. via
`setsid`) or comes from another `podman exec` cannot even reach the
agent, and a client whose session cannot be verified is refused. With
`--no-inject-broker` or in the `flue debug` passthrough the check is off
(a warning is logged; in those modes any other process in the container
can use the session's agent socket). Refusal warnings (session-check
refusals and the live-connection cap) are rate-limited — the first of a
burst in full, a summary every 30 s, the rest flushed at session end —
and `--log-file` sends log output to a file instead of the terminal.

## Integration check (manual)

Run these against a real container to verify the full path works. They all
use `flue enter`, which injects the broker in memory — so any running
container works, with no writable filesystem or image-side broker needed.
Because most of them exercise the full agent rather than a key allowlist,
they pass `--allow-all`; the allowlist checks (#5, #6, #7) instead name a
specific key.

```sh
# 1. ssh-add -l run twice in a row succeeds both times
flue --allow-all enter <container> -- sh -c 'ssh-add -l && ssh-add -l'

# 2. A loop of ~20 concurrent ssh-add -l calls all succeed
flue --allow-all enter <container> -- sh -c 'for i in $(seq 1 20); do ssh-add -l & done; wait'

# 3. ssh-add -D is refused
flue --allow-all enter <container> -- sh -c 'ssh-add -D 2>&1'  # expect an error

# 4. git over ssh works
flue --allow-all enter <container> git ls-remote git@github.com:your/repo.git

# 5. With --allow-key, only that key shows in ssh-add -l
flue --allow-key SHA256:... enter <container> ssh-add -l

# 6. With --allow-key-type, only keys of those types show
flue --allow-key-type ssh-ed25519 enter <container> ssh-add -l

# 7. With --allow-key-file, only the file's keys show (authorized_keys format)
flue --allow-key-file /path/to/authorized_keys enter <container> ssh-add -l

# 8. Two concurrent sessions get distinct sockets and both work
flue --allow-all enter <container> -- sh -c 'ssh-add -l' & flue --allow-all enter <container> -- sh -c 'ssh-add -l'; wait

# 9. (run from a terminal, so the broker is a session leader) a client
#    that starts its own session is refused
flue --allow-all enter <container> -- setsid sh -c 'ssh-add -l 2>&1'  # expect an agent error, not a key list

# 10. The session check is on with and without a tty: the log shows
#    "session check on" in both cases (the tty run in required mode,
#    the other in advisory) — verified 2026-10-12 on podman 5.8.7 /
#    crun 1.28 / Linux 7.2 (the broker's session was private in both).
#    On a runtime that places execs in the container's init session the
#    no-tty run logs the reason the check is off instead; on that
#    runtime `--session-check` is the escape hatch (see Flags).
flue --allow-all enter <container> true            # from a terminal
echo | flue --allow-all enter <container> true     # no tty
```

---

*Yes, it's pronounced the same as the flu. No, we won't be making you soup.*
