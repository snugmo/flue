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
Containerfile       multi-stage image build: golang build stage → brokers stage (both flue-broker binaries, for release attestation) → FROM scratch final stage (just /flue)
README.md           usage, flags, how-it-works, security
```

## Commands

| Task           | Command |
| -------------- | ------- |
| Build binaries | `go build -o cmd/flue/flue-broker.bin ./cmd/flue-broker && go build -o flue ./cmd/flue && cp cmd/flue/flue-broker.bin flue-broker` |
| Test           | `go build -o cmd/flue/flue-broker.bin ./cmd/flue-broker && go test ./...` |
| Build image    | `buildah build --isolation chroot --layers --format docker --build-arg VERSION=<tag> -t <image> -f Containerfile .` (see the Containerfile header; `--build-arg TARGETARCH=<arch>` for arm64) |
| Forward agent  | `flue exec -- podman exec --preserve-fds 1 -e SSH_AUTH_SOCK=/tmp/flue.sock <container> flue-broker -sock /tmp/flue.sock` |
| Enter a shell  | `flue enter [container] [command…]` (defaults: snugmo, `$FLUE_ENTER_CMD` else `/bin/sh`; `-ti` when stdin is a tty; a `--` with no argument before it makes what follows the command in the default container) |

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
- **`cmd/flue` embeds the built broker** (`cmd/flue/flue-broker.bin`,
  go:embed — see `cmd/flue/embed.go`): the broker must be built first (the
  Build/Test commands above do this). If the embed file is missing,
  compiling `cmd/flue` fails with "no matching files found" — a deliberate
  hard failure. The embedded bytes are stored verbatim, so
  `flue extract-broker` produces a broker 100% identical to the build
  source.
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

- **New flag**: add it to the flag registration in `main.go` (cobra's
  `init()` in `flue`, the `flag` block in `flue-broker`); document in the
  README `### Flags` table.
- **Socket path coupling**: the broker generates a unique per-session
  path (XDG_RUNTIME_DIR → /run/user/\<uid\> → /run → /tmp) and exports it
  as `SSH_AUTH_SOCK`; standalone mode must pin `-sock` to a fixed path.
  The standalone examples in AGENTS.md, the Containerfile header, and the
  `flue exec` help text must stay in sync with that rule.
- **New protocol support** (e.g. GPG agent): extend `agentFilter` or add
  a new command under `cmd/`.
- **New image variant**: add a stage to the `Containerfile` (one
  Containerfile serves local and release builds; keep it in sync with
  the Release workflow's `image` job).
