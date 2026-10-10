# AGENTS.md — flue

## Project Overview

**flue** — a narrow channel from your host's SSH agent into a container. Two Go binaries: `flue` (container-side) listens on a Unix socket inside a container and proxies every byte to/from a preserved file descriptor pointing at the host's SSH authentication agent socket; `fluehost` (host-side) opens that agent socket with a proper `socket()`+`connect(2)`, dups it onto FD 3, and `exec`s `podman exec --preserve-fds` so the connected socket is forwarded into the container. From the [Snugmo](https://github.com/snugmo) GitHub org.

## Structure

```
cmd/
  flue/
    main.go            # Container-side proxy: Unix socket listener -> raw unix.Read/Write to preserved FD
  fluehost/
    main.go            # Host-side helper: connect() to agent socket, dup2 onto FD 3, exec command
go.mod                 # Module github.com/snugmo/flue, depends on golang.org/x/sys
go.sum
Containerfile          # Two-stage: builds static binaries in go-build, copies to final image
build.sh               # Buildah wrapper with registry/tag config
README.md              # Usage, flags, how-it-works, security notes
LICENSE                # AGPL-3.0
```

## Essential Commands

| Task                  | Command                          |
| --------------------- | -------------------------------- |
| Build the binaries    | `go build -o flue ./cmd/flue && go build -o fluehost ./cmd/fluehost` |
| Build the image       | `./build.sh`                     |
| Build + push          | `./build.sh push`                |
| Build + push (custom) | `./build.sh push <registry>`     |
| Run the proxy         | `podman exec -e SSH_AUTH_SOCK=/tmp/flue.sock --preserve-fds 1 <container> flue` |
| Forward agent (via fluehost) | `fluehost -- podman exec -e SSH_AUTH_SOCK=/tmp/flue.sock --preserve-fds 1 <container> flue` |

Build overrides: `DEV_VERSION=`, `DEV_IMAGE_NAME=`, `DEV_REGISTRY=`, `DEV_BASE_IMAGE=` (see `build.sh`).

## Key Conventions

### Commit Conventions

- Make small, logically contained commits; one per meaningful change, committed as you go.
- Prefix every commit subject with a single emoji representing the change, followed by the conventional type prefix (`feat:`, `chore:`, `docs:`, `fix:`, …); keep subjects under 72 characters. Examples: `✨ feat: add flue`, `📦 chore: update golang.org/x/sys`, `✏️ docs: …`, `🐛 fix: …`.
- End commit messages with an `Assisted-by: <agent>:<model>` trailer, where `<agent>` is the coding agent doing the committing (no "generated with" line).

### Go Source (`cmd/flue/main.go`)

- **Static binary**: always build with `CGO_ENABLED=0` and `-ldflags="-s -w"` so the binary runs in any Linux container without glibc or shared libraries.
- **Raw unix syscalls for the agent FD**: the preserved FD is a Unix socket. Go's `os.File.Read()` returns `EINVAL` on Unix sockets, so the proxy uses `unix.Read`/`unix.Write` from `golang.org/x/sys/unix` for the agent side. The client side (container-local socket) uses the normal `net.Conn` interface.
- **Bidirectional relay**: two goroutines per connection — one copies client→agent, the other agent→client. Each `done` channel signals the first direction to close; the other follows.
- **No external dependencies** beyond `golang.org/x/sys` (standard-library-adjacent, needed only for the raw socket syscalls).

### Containerfile

- **Two stages**: the `go-build` stage compiles the static binary using the Go toolchain from the cote base image (`/opt/go`). The final stage copies just the binary — the image adds ~2.3 MB over the base.
- **Base image**: defaults to `ghcr.io/snugmo/cote:latest-base` (Fedora 44, non-root user `user` UID 1000). Override with `DEV_BASE_IMAGE`.
- **OCI well-known labels**: `org.opencontainers.image.*` and `io.openshift.tags` are set via `--build-arg` from `build.sh`.
- **Layer caching**: the `COPY go.mod go.sum` + `RUN go build` pattern is not used because the Go build is a single `RUN` that compiles from source; the base image layer is the only cached layer that matters.

### Build Script (`build.sh`)

- Uses `buildah` (not `docker`) — rootless, works in CI without a Docker daemon.
- The tag is the git-describe version (`git describe --tags --always --dirty`, leading `v` stripped, falls back to `latest`), so the image ref is `ghcr.io/snugmo/flue:<tag>` — e.g. `0.1.0`.
- Registry defaults to `ghcr.io/snugmo`; `./build.sh push` publishes to GitHub Container Registry. For local dev / CI use `DEV_REGISTRY=localhost:5000`.
- The base image reference defaults to `${REGISTRY}/cote:${TAG}-base` (the cote base image at the same git-describe tag). Override with `DEV_BASE_IMAGE`.

## Gotchas

1. **Agent forwarding must be active**: `SSH_AUTH_SOCK` must be set in the exec'ing shell and at least one key must be added (`ssh-add -l`). Without this, FD 3 is not a valid agent socket and the proxy will fail on the first I/O.
2. **Trusted containers only**: the proxy adds no access control. Anyone inside the container can use your SSH keys.
3. **Preserved FD number**: defaults to 3 (the first FD after stdin/stdout/stderr). If you have other FDs open in your shell, use `-fd` to target the correct one.
4. **Base image dependency**: the extension build pulls the cote base image from the registry. The base image must be pushed before building flue. Override with `DEV_BASE_IMAGE`.
5. **Static binary**: the binary is built with `CGO_ENABLED=0`. Do not add cgo dependencies — they will break the static build.

## Adding Features

- **New flags**: add to the `flag` block in `main.go`; document in the README `### Flags` table.
- **New protocol support** (e.g., GPG agent, SSH key agent v2): extend the `handle` function or add a new command under `cmd/`.
- **New image variant**: add a stage to the Containerfile or a new build target in `build.sh`.
