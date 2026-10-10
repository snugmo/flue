# flue

A narrow channel from your host's SSH agent into a container.

`flue` is a tiny Go daemon that listens on a Unix
socket inside a container and proxies every byte to/from a preserved file
descriptor pointing at the host's SSH authentication agent socket. This
enables `podman exec --preserve-fds` to share a forwarded SSH agent into
the container.

Built from source at `cmd/flue/` and installed as
`/usr/local/bin/flue` (a fully static binary, no runtime deps).

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
3. The container's image includes the `flue` binary (use this
   image, or build and install the binary into your own image).

### One-liner

```sh
podman exec \
  -e SSH_AUTH_SOCK=/tmp/flue.sock \
  --preserve-fds 1 \
  <container> flue
```

This runs the proxy as the container's foreground process. It listens on
`/tmp/flue.sock` and proxies to FD 3 (the preserved host agent
socket).

> **Note:** `--preserve-fds` forwards *open file descriptors*, not socket
> paths. FD 3 must already hold a connected handle to your agent socket in
> the exec'ing process. The easiest way to do that reliably is the
> companion `fluehost` binary (see below), which opens the agent socket
> with a proper `socket()`+`connect(2)`, dups it onto FD 3, and then
> `exec`s `podman` for you.

### With fluehost (recommended)

`fluehost` is the host-side half. It removes the need for shell fd
redirection, which can fail with `ENXIO` on Unix sockets because the shell
uses `open(2)`'s implicit-connect path:

```sh
go run ./cmd/fluehost -- podman exec \
  --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock \
  <container> flue
```

or build it once and keep it on your `PATH`:

```sh
go build -o ~/bin/fluehost ./cmd/fluehost
fluehost -- podman exec --preserve-fds 1 \
  -e SSH_AUTH_SOCK=/tmp/flue.sock <container> flue
```

`fluehost` reads `$SSH_AUTH_SOCK` (or `-sock`), connects to it, places the
connected socket on FD 3 (or `-fd N`), and `exec`s the given command, which
inherits that FD. `podman --preserve-fds 1` then forwards it into the
container.

### In a shell (background the proxy, get a shell)

```sh
podman exec \
  -e SSH_AUTH_SOCK=/tmp/flue.sock \
  --preserve-fds 1 \
  <container> sh -c '
    flue &
    exec bash
  '
```

Then inside the container:

```sh
$ ssh-add -l
3072 SHA256:xxxx your-key (ED25519)
$ git clone git@github.com:your/repo.git   # works
```

### Flags

| Flag     | Default                      | Description                           |
| -------- | ---------------------------- | ------------------------------------- |
| `-sock`  | `/tmp/flue.sock`             | Unix socket path inside the container |
| `-fd`    | `3`                          | Preserved FD number of the host agent |

## How it works

1. You SSH to the host with agent forwarding enabled. SSH creates a Unix
   socket at `$SSH_AUTH_SOCK` on the host and sets the env var in your
   shell. That socket is an open file descriptor (say FD 3) in your shell
   process.
2. `podman exec --preserve-fds 1` copies FD 3 from the exec'd process into
   the container as FD 3 of the exec'd command.
3. `flue` opens a *new* Unix socket at `/tmp/flue.sock`
   inside the container and shuttles bytes bidirectionally between it and
   FD 3 using raw `unix.Read`/`unix.Write` syscalls.
4. Inside the container, `ssh` / `ssh-add` / `git` see
   `SSH_AUTH_SOCK=/tmp/flue.sock` and talk to the proxy, which
   forwards to the real agent on the host (via the forwarded channel).

## Security

Anyone inside the container can use your SSH keys. Only use this in
trusted containers. The proxy adds no access control; it is a transparent
byte pipe.
