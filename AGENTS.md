# AGENTS.md — flue

## Project Overview

**flue** — a narrow channel from your host's SSH agent into a container. Two Go binaries: `flue-broker` (container-side) listens on a Unix socket inside the container and hands that listening socket's file descriptor to the host once over a control socketpair (fd 3, passed via `podman exec --preserve-fds`); `flue` (host-side) creates that socketpair, execs the container command with one end preserved, receives the listener fd, and `accept()`s each client connection directly, proxying it to the real host agent (`$SSH_AUTH_SOCK`) using `golang.org/x/crypto/ssh/agent` with a key-allowlist filter. From the [Snugmo](https://github.com/snugmo) GitHub org.

## Structure

```
cmd/
  flue-broker/
    main.go            # Container-side: listen on socket, hand listener fd to host, watch control for EOF
  flue/
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
| Build the binaries    | `go build -o flue-broker ./cmd/flue-broker && go build -o flue ./cmd/flue` |
| Build the image       | `./build.sh`                     |
| Build + push          | `./build.sh push`                |
| Build + push (custom) | `./build.sh push <registry>`     |
| Forward agent         | `flue -- podman exec -e SSH_AUTH_SOCK=/tmp/flue.sock --preserve-fds 1 <container> flue-broker` |
| Forward agent + shell | `flue -- podman exec --preserve-fds 1 <container> flue-broker bash` |
| Enter a container     | `flue enter <container> [command…]` (default `/bin/sh`; `-ti` when stdin is a tty) |
| Run tests             | `go test ./...`                  |

Build overrides: `DEV_VERSION=`, `DEV_IMAGE_NAME=`, `DEV_REGISTRY=`, `DEV_BASE_IMAGE=` (see `build.sh`).

## Key Conventions

### Commit Conventions

- Make small, logically contained commits; one per meaningful change, committed as you go.
- Prefix every commit subject with a single emoji, followed by the conventional type prefix (`feat:`, `chore:`, `docs:`, `fix:`, …); keep subjects under 72 characters. Choose whichever single emoji best represents the actual change — there is no fixed set of allowed emojis, and it should vary from commit to commit. Pick an emoji that captures the essence of what changed, not just a stand-in for the type prefix (e.g. `🏷️` for a rename, `🧪` for new tests, `📝` for prose edits — the past history shows a wide range in use). The examples below only illustrate the format, not a menu of choices: `✨ feat: add flue`, `📦 chore: update golang.org/x/sys`, `🐛 fix: handle EOF on the control channel`.
- End commit messages with an `Assisted-by: <agent>:<model>` trailer, where `<agent>` is the coding agent doing the committing (no "generated with" line).

### Go Source (`cmd/flue-broker/main.go`)

- **Static binary**: always build with `CGO_ENABLED=0` and `-ldflags="-s -w"` so the binary runs in any Linux container without glibc or shared libraries.
- **`flag.Parse()` is required**: it is called at the top of `main` (before reading `-sock`/`-fd` or `flag.Args()`). Without it the flags are silently ignored and `flag.Args()` is empty, so wrapper mode would never see a command.
- **SCM_RIGHTS listener-fd handoff**: the broker passes its listening socket's fd to the host over the control socketpair using `unix.UnixRights` + `WriteMsgUnix` exactly once at startup. No per-client fd traffic, no protocol parsing. The control channel is one-shot.
- **Two modes**: standalone (no args) blocks until the host goes away; wrapper (`flue-broker [cmd] [args…]`) runs the command as a child with `SSH_AUTH_SOCK` set to `-sock`, marks the broker's socket fds `CLOEXEC` (via `/proc/self/fd`) so the child doesn't inherit them, and exits with the child's exit status. Host departure is watched best-effort to kill a stray child.
- **No external dependencies** beyond `golang.org/x/sys` (standard-library-adjacent, needed only for the raw socket syscalls). Logging is `log/slog` (stdlib) to stderr, configured by `-log-level` (debug|info|warn|error, default info).

### Go Source (`cmd/flue/main.go`)

- **Subcommand dispatch**: the first argument after the flags selects the mode. `flue enter <container> [command…]` builds and runs `podman exec [-ti] --preserve-fds 1 -e SSH_AUTH_SOCK=/tmp/flue.sock <container> flue-broker [command…]` — the in-container socket path is fixed (`enterSocket` const, must match the broker's `-sock` default; guarded by `TestEnterSocketMatchesBrokerDefault`) and the command defaults to `/bin/sh` (`defaultEnterCmd`); a tty is allocated only when stdin is a tty (`stdinIsTTY` via the TCGETS ioctl). Any other first argument is the classic passthrough mode (a program literally named `enter` can only be run that way by giving its path). Both modes share the socketpair machinery in `runChildWithAgent`, which returns the child's exit code.
- **Socketpair control channel**: `SOCK_SEQPACKET` socketpair; one end is passed to the container as fd 3 via `exec.Command.ExtraFiles`, the other end stays on the host. It carries exactly one message (the listener fd), read by `receiveListener`; the connection itself stays open until the broker is gone (so the broker never sees a premature "host gone") and is closed in `runChildWithAgent` after `cmd.Wait()`.
- **Lifetime via the process tree**: flue blocks on `cmd.Wait()` of its child (the `podman exec` running the broker) rather than on control-channel EOF — the netpoller does not reliably deliver EOF for a closed socketpair end in all environments. In wrapper mode the broker exits when the wrapped command ends, so flue returns that command's exit status; in standalone mode it runs until signalled. The accept loop runs in a background goroutine and is stopped by closing the listener.
- **Per-client upstream**: each accepted client connection gets a fresh `net.Dial("unix", SSH_AUTH_SOCK)` connection, wrapped in an `agentFilter` that implements `agent.ExtendedAgent`.
- **Key allowlist**: `--allow-key` (repeatable) filters `List` and `Sign`/`SignWithFlags` by SHA256 fingerprint. `Add`, `Remove`, `RemoveAll`, `Lock`, `Unlock`, `Signers` are always denied. Only the `session-bind@openssh.com` extension is forwarded.
- **Runtime dependencies**: `golang.org/x/sys` (unix syscalls) + `golang.org/x/crypto` (ssh agent protocol). Logging is `log/slog` (stdlib) to stderr, configured by `-log-level` (debug|info|warn|error, default info); the per-request agent trace (formerly `-verbose`) is emitted at `debug`. **Test-only dependencies**: `go.uber.org/goleak` (goroutine leak detection) + `github.com/google/go-cmp` (readable diffs in test failures).

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

1. **Agent forwarding must be active**: `SSH_AUTH_SOCK` must be set in the exec'ing shell and at least one key must be added (`ssh-add -l`). Without this, the host agent socket is not available and `flue` will fail on the first I/O.
2. **Trusted containers only**: the filter restricts which keys can sign, but anyone inside the container can see the listed keys. Use `--allow-key` to limit exposure.
3. **Preserved FD number**: defaults to 3 (the first FD after stdin/stdout/stderr). If you have other FDs open in your shell, use `-fd` to target the correct one.
4. **Base image dependency**: the extension build pulls the cote base image from the registry. The base image must be pushed before building flue. Override with `DEV_BASE_IMAGE`.
5. **Static binary**: the binary is built with `CGO_ENABLED=0`. Do not add cgo dependencies — they will break the static build.

### Development-time gotchas

1. **No `unix.IsTerminal` in this x/sys**: `golang.org/x/sys@v0.49.0` (see go.mod) has no `IsTerminal` helper — that API is not in this version. To test whether an fd is a tty, use the `TCGETS` ioctl: `unix.IoctlGetTermios(fd, unix.TCGETS)`. Note its signature here is the two-arg form returning `(*Termios, error)`; there is no three-arg form.
2. **slog does not interpret format verbs**: `log.Error("msg: %v", err)` logs the literal `msg: %v` and dumps `err` as a `!BADKEY=…` key/value pair, because slog treats variadic args as key/value pairs, not printf args. Use the `fatalf` helper (it Sprintfs first) or format explicitly. This bit both binaries.

## Adding Features

- **New flags**: add to the `flag` block in `main.go`; document in the README `### Flags` table.
- **New protocol support** (e.g., GPG agent, SSH key agent v2): extend the `agentFilter` or add a new command under `cmd/`.
- **New image variant**: add a stage to the Containerfile or a new build target in `build.sh`.
