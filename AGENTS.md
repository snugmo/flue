# AGENTS.md — flue

**flue** — a filtered SSH agent bridge from your host into a container.
Two Go binaries: `flue-broker` (container-side) listens on a Unix socket
and hands that listener's fd to the host once over a control socketpair
(fd 3, via `podman exec --preserve-fds`); `flue` (host-side) creates the
socketpair, execs the container command, receives the listener, and
proxies each client connection to the real host agent
(`$SSH_AUTH_SOCK`) through a key-allowlist filter. From the
[Snugmo](https://github.com/snugmo) GitHub org.

## Structure

```
cmd/flue/           host side: socketpair control channel, filtered agent proxy
cmd/flue-broker/    container side: thin listener + one-shot listener-fd handoff
docs/               on-demand details: flue.md, flue-broker.md, build.md, duplication-policy.md
Containerfile       two-stage build of the static binaries
build.sh            buildah wrapper (registry / tag config)
README.md           usage, flags, how-it-works, security
```

## Commands

| Task           | Command |
| -------------- | ------- |
| Build binaries | `go build -o flue-broker ./cmd/flue-broker && go build -o flue ./cmd/flue` |
| Test           | `go test ./...` |
| Build image    | `./build.sh` · `./build.sh push [registry]` · overrides `DEV_VERSION`, `DEV_IMAGE_NAME`, `DEV_REGISTRY`, `DEV_BASE_IMAGE` |
| Forward agent  | `flue -- podman exec -e SSH_AUTH_SOCK=/tmp/flue.sock --preserve-fds 1 <container> flue-broker` |
| Enter a shell  | `flue enter <container> [command…]` (default `/bin/sh`; `-ti` when stdin is a tty) |

## Repo-wide rules

- **Static binaries**: always build `CGO_ENABLED=0` with
  `-ldflags="-s -w"`. Never add cgo dependencies — they break the static
  build.
- **slog does not interpret format verbs**: variadic args are key/value
  pairs, so `log.Error("msg: %v", err)` logs a `!BADKEY` pair. Use the
  `fatalf` helper for fatal errors (plain text to stderr + exit 1, it
  Sprintfs first) and format the string explicitly for slog messages.
- **`golang.org/x/sys` is pinned at v0.49.0**: it has no
  `unix.IsTerminal`; for a tty check use
  `unix.IoctlGetTermios(fd, unix.TCGETS)` (two-arg form).
- **The file headers in `cmd/*/main.go` are the authoritative
  documentation** of each binary — read them before changing that
  binary.
- **Do not "fix" the deliberate duplication** into a shared package:
  [docs/duplication-policy.md](docs/duplication-policy.md).
- Component details are on-demand: [docs/flue.md](docs/flue.md) (host),
  [docs/flue-broker.md](docs/flue-broker.md) (container),
  [docs/build.md](docs/build.md) (image).

## Commit conventions

- Small commits, one per meaningful change, committed as you go.
- Subject: `<emoji> <type>: <summary>` — a conventional type prefix
  (`feat:`, `fix:`, `chore:`, `docs:`, …) and a summary under 72 chars.
  The single emoji is chosen per change to capture what actually changed;
  there is no fixed set of allowed emojis.
- End the message with an `Assisted-by: <agent>:<model>` trailer (no
  "generated with" line).

## Adding features

- **New flag**: add to the `flag` block in `main.go`; document in the
  README `### Flags` table.
- **Socket path coupling**: the in-container path is fixed —
  `enterSocket` in `cmd/flue` must match the broker's `-sock` default
  (guarded by `TestEnterSocketMatchesBrokerDefault`).
- **New protocol support** (e.g. GPG agent): extend `agentFilter` or add
  a new command under `cmd/`.
- **New image variant**: add a Containerfile stage or a `build.sh`
  target.
