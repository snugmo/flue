# flue

A narrow channel from your host's SSH agent into a container, with host-side
key filtering.

Two small Go binaries:

- **`flue`** (host-side) creates a Unix socketpair, passes one end into the
  container, receives the broker's listening fd, and proxies each client
  connection to the real `$SSH_AUTH_SOCK` with a key-allowlist filter. Its
  **`flue enter`** subcommand is the usual way in: it runs the `podman exec`
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
flue enter <container>     # or just flue enter — snugmo is the default
```

…and you are in a shell where `ssh-add -l`, `git push`, and friends use the
host's agent — with only the keys you allowlisted visible.

Built from source at `cmd/flue-broker/` and `cmd/flue/` and installed as
`/usr/local/bin/flue-broker` and `/usr/local/bin/flue` (fully static
binaries, no runtime deps).

> **Field-verified end to end.** The full path has been exercised for real:
> laptop `ssh-agent` → host workstation → a snugmo container, where
> `git push` over SSH ran with the askpass prompt surfacing on the laptop's
> Wayland desktop. Signing round-trips (not just `ssh-add -l`) work across
> podman, with only the allowlisted key visible in the container.

## Usage

### Prerequisites

1. SSH agent forwarding is active in your session on the host
   (`SSH_AUTH_SOCK` is set, `ssh-add -l` lists keys).
2. The container is running.

That is all for the usual flow: by default `flue enter` injects the broker
into the container in memory, so the container needs neither a writable
filesystem nor a `flue-broker` in its image. (If you disable injection —
see [Flags](#flags) — the image must provide `flue-broker`; see
[Getting a broker into a container](#getting-a-broker-into-a-container).)

### Getting in: `flue enter`

`flue enter` runs the `podman exec` for you: it preserves the control
socket as fd 3, hands your command to `flue-broker` in wrapper mode, runs
`/bin/sh` when you give no command, targets the `snugmo` container when
you give none, and allocates a tty when your stdin is a tty:

```sh
flue enter                                  # /bin/sh in snugmo
flue enter <container>                      # /bin/sh inside the container
flue enter <container> bash                 # bash
flue enter <container> git ls-remote git@github.com:you/repo.git
flue enter <container> -- bash -l           # command with its own flags
```

The default command is `/bin/sh`. Set `$FLUE_ENTER_CMD` to run something
else by default (the value is split on whitespace; a command given on the
command line always wins):

```sh
FLUE_ENTER_CMD=bash flue enter           # bash in snugmo
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
touches the container's filesystem. It works in a container with no
writable filesystem and no `flue-broker` in the image at all. Disable
injection to use the image's `flue-broker` instead:

```sh
flue enter --no-inject-broker <container>       # or --inject-broker=false
FLUE_INJECT_BROKER=0 flue enter <container>     # via the environment
```

Global flags (`--sock`, `--allow-key`, `--log-level`, `--log-file`) go
before the
subcommand: `flue --allow-key SHA256:… enter <container>`. (The subcommand
`--` separator above is for your *target command's* flags.)

### Flags

**flue** (host-side; persistent flags, placed before the subcommand):

| Flag          | Default                        | Description                                                                                                                                                                                              |
| ------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--sock`      | `$SSH_AUTH_SOCK`               | Agent socket path to proxy to                                                                                                                                                                            |
| `--allow-key` | (none = all keys)              | Repeatable. Only keys with this SHA256 fingerprint are listed and can sign. Fingerprints are compared the way `ssh-add -l` prints them — for a certificate, that is the fingerprint of its underlying key|
| `--log-level` | `$FLUE_LOG_LEVEL`, else `info` | Log level: `debug`, `info`, `warn`, or `error`. `debug` logs every agent request (operation, fingerprint, allowed/denied). When the flag is unset, `$FLUE_LOG_LEVEL` is read; a set flag wins            |
| `--log-file`  | `stderr`                       | Append log output to this file (created if needed, mode `0600`) instead of stderr — keeps session-check refusal warnings off the user's terminal                                                                                              |

**enter** (enter-scoped flag and arguments):

| Flag              | Default                  | Description                                                                                                                                                                                                                                             |
| ----------------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| *(arguments)*     | `snugmo`, `$FLUE_ENTER_CMD` else `/bin/sh` | Positional: the container to exec into (default `snugmo`), then the command to run inside it (default `$FLUE_ENTER_CMD` when set and non-blank — split on whitespace — else `/bin/sh`) |
| `--inject-broker` | `true`  | Run the broker from an in-memory `memfd` (exec'd via `/proc/self/fd/4`) instead of the image's `flue-broker`; no writable filesystem needed. Disable with `--no-inject-broker`, `--inject-broker=false`, or `FLUE_INJECT_BROKER=0` (a set flag wins over the variable) |

**flue-broker** (container-side):

| Flag         | Default                               | Description                                                                                                                                                                                                                      |
| ------------ | ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-sock`      | unique per session (`flue-<8hex>.sock`) | Unix socket path to listen on. When unset (or empty), a unique path is generated in the first writable of `$XDG_RUNTIME_DIR`, `/run/user/<uid>`, `/run`, `/tmp`; set it (e.g. to `/tmp/flue.sock`) to pin a fixed path |
| `-fd`        | `3`                                   | File descriptor number of the preserved control socket                                                                                                                                                                           |
| `-log-level` | `info`                                | Log level: `debug`, `info`, `warn`, or `error`                                                                                                                                                                                   |

After the flags, an optional command starts wrapper mode: `flue-broker
[command] [args…]` runs that command as a child with `SSH_AUTH_SOCK` set to
the broker's socket. No command = standalone mode, which blocks until the
host goes away.

These are [cobra](https://github.com/spf13/cobra) persistent flags, so they
accept the `--flag value` or `--flag=value` forms and go before the
subcommand (`flue --sock /path exec -- …`). A subcommand's arguments follow
the subcommand name; a `--` separates them from the target command so the
target's flags pass through verbatim.

### Manual: `flue exec` (passthrough)

If you prefer to assemble the `podman exec` yourself, or the target is not
podman, `flue exec` runs any command that receives the control socketpair
(over fd 3). Wrapper mode is the normal shape — the broker sets
`SSH_AUTH_SOCK` for its child, so no `-e` is needed:

```sh
flue exec -- podman exec --preserve-fds 1 <container> flue-broker bash
```

In standalone mode (no command after `flue-broker`) nothing sets
`SSH_AUTH_SOCK` for you, so pin `-sock` to a fixed path and export the same
one:

```sh
flue exec -- podman exec --preserve-fds 1 \
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
has it (the image built by this repo installs it at
`/usr/local/bin/flue-broker`) or by installing a copy:

```sh
flue extract-broker /tmp/flue-broker
podman cp /tmp/flue-broker <container>:/usr/local/bin/flue-broker
```

`flue` embeds the `flue-broker` built at the same time (`go:embed`), so
`extract-broker` writes out a byte-identical copy (mode 0755) — built for
the same architecture as your `flue`, and identical to the image's own
broker when `flue` comes from the flue image. Unlike `exec` and `enter`,
`extract-broker` needs no agent socket and takes no flags.

## Build

```bash
./build.sh
./build.sh push <registry>
```

Build order: `flue-broker` is built first, to the `go:embed` path
`cmd/flue/flue-broker.bin`, because `flue` embeds that binary (compiling
`flue` fails if the file is missing — a deliberate hard failure).
`build.sh` then builds `flue` and copies the same broker file to the repo
root; the Containerfile copies it to the image's
`/usr/local/bin/flue-broker`, so a `flue` from the image embeds a broker
identical to the image's own.

### Environment Variables

| Variable         | Default                  | Description          |
| ---------------- | ------------------------ | -------------------- |
| `DEV_REGISTRY`   | `ghcr.io/snugmo`         | Target registry      |
| `DEV_IMAGE_NAME` | `flue`                   | Image name           |
| `DEV_VERSION`    | git tag                  | Image tag            |
| `DEV_BASE_IMAGE` | auto                     | Base image reference |

## How it works

1. `flue` creates a `SOCK_SEQPACKET` Unix socketpair.
2. It passes one end into the container as fd 3 via `podman exec
   --preserve-fds` (2 when the broker memfd is also passed as fd 4), closes
   its copy, and keeps the other end as the control channel.
3. Inside the container, `flue-broker` listens on a socket — a unique
   per-session path unless `-sock` pins one — and sends that listening
   socket's fd to the host over the control socket (SCM_RIGHTS) exactly
   once. It then either (standalone) blocks watching for the host to go
   away, or (wrapper mode) launches the wrapped command as a child with
   `SSH_AUTH_SOCK` pointed at its socket and waits for it. Because the
   host end of the control socket has `SO_PASSCRED` enabled, the same
   message also carries the broker's credentials.
4. On the host, `flue` receives the listener fd and `accept()`s client
   connections directly; for each one it dials a fresh connection to
   `$SSH_AUTH_SOCK` and runs `agent.ServeAgent(filter, clientConn)` —
   the filter wraps the upstream agent client and enforces the key
   allowlist. From the handover message's `SCM_CREDENTIALS` it learns
   the broker's session id. When that session is private (its only
   live member at handover — what an exec that created its own session
   looks like, with or without a tty), each accepted client must share
   it (`SO_PEERCRED` → `/proc`); clients from another session are
   refused. For an interactive `flue enter` (injected broker with a
   tty) the check is required: if it cannot be enabled, `flue` aborts
   the session rather than serve it without the check. With
   `--no-inject-broker` or in the `flue exec` passthrough the broker is
   untrusted code and the check is not used (a warning is logged); in
   the remaining mode (an injected broker without a tty) its state is
   logged at `info` when the session starts.
5. Lifetime is driven by the process tree, not the control channel (which
   carries a single listener-fd message). In wrapper mode, when the wrapped
   command exits, `flue-broker` exits, the `podman exec` returns, and
   `flue` — which is blocked on that child — tears down the listener and
   exits with the command's exit status. In standalone mode `flue` runs
   until it is signalled.

### Filter policy

| Operation       | Behavior                                              |
| --------------- | ----------------------------------------------------- |
| `List`          | Forwarded; only allowed keys returned if `--allow-key` is set |
| `Sign`          | Forwarded only for allowed keys                       |
| `SignWithFlags` | Forwarded only for allowed keys (needed for RSA rsa-sha2-256/512) |
| `Add`           | Denied                                                |
| `Remove`        | Denied                                                |
| `RemoveAll`     | Denied                                                |
| `Lock`          | Denied                                                |
| `Unlock`        | Denied                                                |
| `Signers`       | Denied                                                |
| `Extension`     | Only `session-bind@openssh.com` is forwarded; all others return `ErrExtensionUnsupported` |

## Security

While a session is open, a process in the container can (a) list the
allowlisted keys — fingerprint and comment are visible — and (b) get
signatures from them. It cannot add, remove, lock, or unlock keys, nor use
any key outside the `--allow-key` list (the filter policy table above
carries the per-operation detail), and it never sees a path or fd to the
host agent socket, its directory, or `$XDG_RUNTIME_DIR`. The trust boundary
is the allowlist, not the container.

When the broker's session is private — its only live member at
handover time, what an exec that created its own session looks like,
with or without a tty — a second gate applies: every client must share
the broker's session, so a rogue process that starts its own session
(for example via `setsid`) or comes from another `podman exec` cannot
even reach the agent, and a client whose session cannot be verified is
refused. When the exec's process shares a session with the container's
other processes (older runtimes), this check does not apply. The check's
mode depends on how the broker was started: for an interactive `flue
enter` (injected broker with a tty) it is required — if it cannot be
enabled, the session is aborted rather than served without the check;
with `--no-inject-broker` or in the `flue exec` passthrough it is not
used at all, because the broker is then untrusted code that could hand
over a listener from any session it controls (a warning is logged);
otherwise (an injected broker without a tty) it is advisory and its
state is logged at `info` when the session starts. Refusal warnings are
rate-limited (the first of a burst in full, a summary every 30 s, the
rest flushed at session end), and `--log-file` sends log output to a
file instead of the terminal.

## Integration check (manual)

Run these against a real container to verify the full path works. They all
use `flue enter`, which injects the broker in memory — so any running
container works, with no writable filesystem or image-side broker needed:

```sh
# 1. ssh-add -l run twice in a row succeeds both times
flue enter <container> -- sh -c 'ssh-add -l && ssh-add -l'

# 2. A loop of ~20 concurrent ssh-add -l calls all succeed
flue enter <container> -- sh -c 'for i in $(seq 1 20); do ssh-add -l & done; wait'

# 3. ssh-add -D is refused
flue enter <container> -- sh -c 'ssh-add -D 2>&1'  # expect an error

# 4. git over ssh works
flue enter <container> git ls-remote git@github.com:your/repo.git

# 5. With --allow-key, only that key shows in ssh-add -l
flue --allow-key SHA256:... enter <container> ssh-add -l

# 6. Two concurrent sessions get distinct sockets and both work
flue enter <container> -- sh -c 'ssh-add -l' & flue enter <container> -- sh -c 'ssh-add -l'; wait

# 7. (run from a terminal, so the broker is a session leader) a client
#    that starts its own session is refused
flue enter <container> -- setsid sh -c 'ssh-add -l 2>&1'  # expect an agent error, not a key list
```

---

*Yes, it's pronounced the same as the flu. No, we won't be making you soup.*
