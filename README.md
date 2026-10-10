# flue

A narrow channel from your host's SSH agent into a container, with host-side
key filtering.

Two small Go binaries:

- **`flue-broker`** (container-side) listens on a Unix socket inside the
  container and hands that listening socket's file descriptor to the host
  once over a socketpair control channel (SCM_RIGHTS). It can run standalone
  (then just watches for host shutdown) or in wrapper mode, where it
  launches a child command (e.g. a shell) with `SSH_AUTH_SOCK` pointed at its
  socket. No protocol parsing here.
- **`flue`** (host-side) creates that socketpair, passes one end into the
  container via `podman exec --preserve-fds 1`, receives the listener
  fd, `accept()`s each client connection itself, and proxies it to the
  real `$SSH_AUTH_SOCK` using
  `golang.org/x/crypto/ssh/agent` with a filtering wrapper.

Built from source at `cmd/flue-broker/` and `cmd/flue/` and installed as
`/usr/local/bin/flue-broker` and `/usr/local/bin/flue` (fully static
binaries, no runtime deps).

> **Field-verified end to end.** The full path has been exercised for real:
> laptop `ssh-agent` → host workstation → a snugmo container, where
> `git push` over SSH ran with the askpass prompt surfacing on the laptop's
> Wayland desktop. Signing round-trips (not just `ssh-add -l`) work across
> podman, with only the allowlisted key visible in the container.

## Build

```bash
./build.sh
./build.sh push <registry>
```

### Environment Variables

| Variable         | Default                  | Description          |
| ---------------- | ------------------------ | -------------------- |
| `DEV_REGISTRY`   | `ghcr.io/snugmo`         | Target registry      |
| `DEV_IMAGE_NAME` | `flue`                   | Image name           |
| `DEV_VERSION`    | git tag                  | Image tag            |
| `DEV_BASE_IMAGE` | auto                     | Base image reference |

## Usage

### Prerequisites

1. SSH agent forwarding is active in your session on the host
   (`SSH_AUTH_SOCK` is set, `ssh-add -l` lists keys).
2. The container is running.
3. The container's image includes the `flue-broker` binary (use this
   image, or build and install the binary into your own image).

### With flue (recommended)

`flue` is the host-side half. It creates a socketpair, passes one end into
the container as fd 3, and proxies each accepted client connection to the
real agent with filtering:

```sh
flue -- podman exec \
  --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock \
  <container> flue-broker
```

or build it once and keep it on your `PATH`:

```sh
go build -o ~/bin/flue ./cmd/flue
flue -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> flue-broker
```

`flue` dials `$SSH_AUTH_SOCK` (or `-sock`) once per client connection, so
repeated `ssh` / `ssh-add` / `git` invocations all work.

### In a shell (wrapper mode)

The cleanest way to get a shell (or any command) inside the container with
the agent available is wrapper mode: hand `flue-broker` a command and it runs
it as a child with `SSH_AUTH_SOCK` set for you. When the command exits,
`flue` returns its exit code.

```sh
flue -- podman exec --preserve-fds 1 <container> flue-broker bash
```

Then inside the container:

```sh
$ ssh-add -l
256 SHA256:xxxx your-key (ED25519)
$ git clone git@github.com:your/repo.git   # works
$ exit
# your host prompt returns, with the shell's exit code
```

Any command works, not just a shell — e.g. `flue-broker git ls-remote
git@github.com:you/repo.git`. `-e SSH_AUTH_SOCK=…` is not needed: the broker
sets it for the child. (The older `sh -c 'flue-broker & exec bash'` form
still works, but wrapper mode replaces it.)

### Flags

**flue-broker** (container-side):

| Flag         | Default          | Description                                          |
| ------------ | ---------------- | ---------------------------------------------------- |
| `-sock`      | `/tmp/flue.sock` | Unix socket path inside the container                |
| `-fd`        | `3`              | File descriptor number of the preserved control socket |
| `-log-level` | `info`           | Log level: `debug`, `info`, `warn`, or `error`       |

After the flags, an optional command starts wrapper mode: `flue-broker
[command] [args…]` runs that command as a child with `SSH_AUTH_SOCK` set to
`-sock` (see wrapper mode above). No command = standalone mode.

**flue** (host-side):

| Flag         | Default            | Description                                          |
| ------------ | ------------------ | ---------------------------------------------------- |
| `-sock`      | `$SSH_AUTH_SOCK`   | Agent socket path to proxy to                        |
| `-allow-key` | (none = all keys)  | Repeatable. Only keys with this SHA256 fingerprint are listed and can sign |
| `-log-level` | `info`             | Log level: `debug`, `info`, `warn`, or `error`. `debug` logs every agent request (operation, fingerprint, allowed/denied) |

## How it works

1. `flue` creates a `SOCK_SEQPACKET` Unix socketpair.
2. It passes one end into the container as fd 3 via `podman exec
   --preserve-fds 1`, closes its copy, and keeps the other end as the
   control channel.
3. Inside the container, `flue-broker` listens on `/tmp/flue.sock` and
   sends that listening socket's fd to the host over the control socket
   (SCM_RIGHTS) exactly once. It then either (standalone) blocks watching
   for the host to go away, or (wrapper mode) launches the wrapped command
   as a child with `SSH_AUTH_SOCK` pointed at its socket and waits for it.
4. On the host, `flue` receives the listener fd and `accept()`s client
   connections directly; for each one it dials a fresh connection to
   `$SSH_AUTH_SOCK` and runs `agent.ServeAgent(filter, clientConn)` —
   the filter wraps the upstream agent client and enforces the key
   allowlist.
5. Lifetime is driven by the process tree, not the control channel (which
   carries a single listener-fd message). In wrapper mode, when the wrapped
   command exits, `flue-broker` exits, the `podman exec` returns, and `flue`
   — which is blocked on that child — tears down the listener and exits with
   the command's exit status. In standalone mode `flue` runs until it is
   signalled.

### Filter policy

| Operation       | Behavior                                              |
| --------------- | ----------------------------------------------------- |
| `List`          | Forwarded; only allowed keys returned if `-allow-key` is set |
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

The container only receives the control socket — never a path or fd to the
host agent socket, its directory, or `$XDG_RUNTIME_DIR`. The filter
restricts which keys can be listed and used for signing, but the key
*material* (fingerprint, comment) of allowed keys is visible inside the
container. Only use this in trusted containers.

## Integration check (manual)

Run these against a real container to verify the full path works:

```sh
# 1. ssh-add -l run twice in a row succeeds both times
flue -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> sh -c '
    flue-broker & sleep 0.5
    ssh-add -l && ssh-add -l
  '

# 2. A loop of ~20 concurrent ssh-add -l calls all succeed
flue -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> sh -c '
    flue-broker & sleep 0.5
    for i in $(seq 1 20); do ssh-add -l & done; wait
  '

# 3. ssh-add -D is refused
flue -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> sh -c '
    flue-broker & sleep 0.5
    ssh-add -D 2>&1  # expect an error
  '

# 4. ssh -T git@<forge> or git ls-remote over ssh works
flue -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> sh -c '
    flue-broker & sleep 0.5
    git ls-remote git@github.com:your/repo.git
  '

# 5. With -allow-key, only that key shows in ssh-add -l
flue -allow-key SHA256:... -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> sh -c '
    flue-broker & sleep 0.5
    ssh-add -l  # only the allowed key is listed
  '
```

---

*Yes, it's pronounced the same as the flu. No, we won't be making you soup.*
