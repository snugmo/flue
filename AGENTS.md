# AGENTS.md — flue

## Project Overview

**flue** — a narrow channel from your host's SSH agent into a container. Two Go binaries: `flue` (container-side) listens on a Unix socket inside the container and forwards each client connection to the host over a control socketpair (fd 3, passed via `podman exec --preserve-fds`); `fluehost` (host-side) creates that socketpair, execs the container command with one end preserved, and relays every forwarded client connection to the real host agent (`$SSH_AUTH_SOCK`) using `golang.org/x/crypto/ssh/agent` with a key-allowlist filter. From the [Snugmo](https://github.com/snugmo) GitHub org.

## Structure

```
cmd/
  flue/
    main.go            # Container-side: Unix socket listener -> SCM_RIGHTS fd pass to host
  fluehost/
    main.go            # Host-side: socketpair control channel, agent proxy with filtering
    filter_test.go     # Tests for the agent filter (list, sign, deny mutating ops)
go.mod                 # Module github.com/snugmo/flue; runtime deps: golang.org/x/sys + x/crypto; test deps: goleak + go-cmp
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
| Forward agent         | `fluehost -- podman exec -e SSH_AUTH_SOCK=/tmp/flue.sock --preserve-fds 1 <container> flue` |
| Run tests             | `go test ./...`                  |

Build overrides: `DEV_VERSION=`, `DEV_IMAGE_NAME=`, `DEV_REGISTRY=`, `DEV_BASE_IMAGE=` (see `build.sh`).

## Key Conventions

### Commit Conventions

- Make small, logically contained commits; one per meaningful change, committed as you go.
- Prefix every commit subject with a single emoji representing the change, followed by the conventional type prefix (`feat:`, `chore:`, `docs:`, `fix:`, …); keep subjects under 72 characters. Examples: `✨ feat: add flue`, `📦 chore: update golang.org/x/sys`, `✏️ docs: …`, `🐛 fix: …`.
- End commit messages with an `Assisted-by: <agent>:<model>` trailer, where `<agent>` is the coding agent doing the committing (no "generated with" line).

### Go Source (`cmd/flue/main.go`)

- **Static binary**: always build with `CGO_ENABLED=0` and `-ldflags="-s -w"` so the binary runs in any Linux container without glibc or shared libraries.
- **SCM_RIGHTS fd pass**: the container-side proxy receives each client connection and sends its fd to the host over the control socketpair using `unix.UnixRights` + `WriteMsgUnix`. No protocol parsing happens here.
- **No external dependencies** beyond `golang.org/x/sys` (standard-library-adjacent, needed only for the raw socket syscalls).

### Go Source (`cmd/fluehost/main.go`)

- **Socketpair control channel**: `SOCK_SEQPACKET` socketpair; one end is passed to the container as fd 3 via `exec.Command.ExtraFiles`, the other end stays on the host.
- **Per-client upstream**: each forwarded client gets a fresh `net.Dial("unix", SSH_AUTH_SOCK)` connection, wrapped in an `agentFilter` that implements `agent.ExtendedAgent`.
- **Key allowlist**: `--allow-key` (repeatable) filters `List` and `Sign`/`SignWithFlags` by SHA256 fingerprint. `Add`, `Remove`, `RemoveAll`, `Lock`, `Unlock`, `Signers` are always denied. Only the `session-bind@openssh.com` extension is forwarded.
- **Runtime dependencies**: `golang.org/x/sys` (unix syscalls) + `golang.org/x/crypto` (ssh agent protocol). **Test-only dependencies**: `go.uber.org/goleak` (goroutine leak detection) + `github.com/google/go-cmp` (readable diffs in test failures).

### Containerfile

- **Two stages**: the `go-build` stage compiles the static binaries using the Go toolchain from the cote base image (`/opt/go`). The final stage copies just the binaries — the image adds ~2.3 MB over the base.
- **Base image**: `build.sh` defaults to `${REGISTRY}/cote:${TAG}-base` (the cote base image at the same git-describe tag). Override with `DEV_BASE_IMAGE`. The Containerfile ARG itself defaults to `ghcr.io/snugmo/cote:latest-base` if invoked standalone.
- **OCI well-known labels**: `org.opencontainers.image.*` and `io.openshift.tags` are set via `--build-arg` from `build.sh`.
- **Layer caching**: `COPY go.mod go.sum` followed by `RUN go build` gives dependency-level caching; the base image layer is the only cached layer that matters in practice since the build is a single step.

### Build Script (`build.sh`)

- Uses `buildah` (not `docker`) — rootless, works in CI without a Docker daemon.
- The tag is the git-describe version (`git describe --tags --always --dirty`, leading `v` stripped, falls back to `latest`), so the image ref is `ghcr.io/snugmo/flue:<tag>` — e.g. `0.1.0`.
- Registry defaults to `ghcr.io/snugmo`; `./build.sh push` publishes to GitHub Container Registry. For local dev / CI use `DEV_REGISTRY=localhost:5000`.
- The base image reference defaults to `${REGISTRY}/cote:${TAG}-base` (the cote base image at the same git-describe tag). Override with `DEV_BASE_IMAGE`.

## Gotchas

1. **Agent forwarding must be active**: `SSH_AUTH_SOCK` must be set in the exec'ing shell and at least one key must be added (`ssh-add -l`). Without this, the host agent socket is not available and `fluehost` will fail on the first I/O.
2. **Trusted containers only**: the filter restricts which keys can sign, but anyone inside the container can see the listed keys. Use `--allow-key` to limit exposure.
3. **Preserved FD number**: defaults to 3 (the first FD after stdin/stdout/stderr). If you have other FDs open in your shell, use `-fd` to target the correct one.
4. **Base image dependency**: the extension build pulls the cote base image from the registry. The base image must be pushed before building flue. Override with `DEV_BASE_IMAGE`.
5. **Static binary**: the binary is built with `CGO_ENABLED=0`. Do not add cgo dependencies — they will break the static build.

## Adding Features

- **New flags**: add to the `flag` block in `main.go`; document in the README `### Flags` table.
- **New protocol support** (e.g., GPG agent, SSH key agent v2): extend the `agentFilter` or add a new command under `cmd/`.
- **New image variant**: add a stage to the Containerfile or a new build target in `build.sh`.
