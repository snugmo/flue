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
cmd/flue/           host side: socketpair control channel, filtered agent proxy; embeds the built broker binary and its sigstore bundle (go:embed)
cmd/flue-broker/    container side: thin listener + one-shot listener-fd handoff
docs/               on-demand details: flue.md, flue-broker.md, build.md, duplication-policy.md
Containerfile       multi-stage image build: broker golang stage → flue-broker scratch stage (one per target platform; the go:embed source, and the broker attestation input) → flue golang stage → FROM scratch final stage (just /flue)
README.md           usage, flags, how-it-works, security
```

## Commands

| Task           | Command |
| -------------- | ------- |
| Build binaries | `go build -o cmd/flue/flue-broker.bin ./cmd/flue-broker && (test -f cmd/flue/flue-broker.sigstore.json || cp cmd/flue/flue-broker.sigstore.json.placeholder cmd/flue/flue-broker.sigstore.json) && go build -o flue ./cmd/flue && cp cmd/flue/flue-broker.bin flue-broker` (the parenthesized step places the placeholder sigstore-bundle embed source unless a signed one is already there — see the embed bullet below) |
| Test           | `go build -o cmd/flue/flue-broker.bin ./cmd/flue-broker && (test -f cmd/flue/flue-broker.sigstore.json || cp cmd/flue/flue-broker.sigstore.json.placeholder cmd/flue/flue-broker.sigstore.json) && go test ./...` |
| Build image    | `buildah build --isolation chroot --layers --format docker --build-arg VERSION=<tag> -t <image> -f Containerfile .` (see the Containerfile header; the image is the host's architecture by default, or `--platform linux/<arch>` for another — never `TARGETOS`/`TARGETARCH` build-args: they would override the per-platform builtins for every platform) |
| Forward agent  | `flue exec -- podman exec --preserve-fds 1 -e SSH_AUTH_SOCK=/tmp/flue.sock <container> flue-broker -sock /tmp/flue.sock` |
| Enter a shell  | `flue enter [container] [command…]` (defaults: snugmo; with no command, a shell the broker resolves in the container — `$SHELL`, `/bin/bash`, `/bin/sh`, then `bash`/`sh` in `$PATH` — unless `$FLUE_ENTER_CMD` is set; `-ti` when stdin is a tty; a `--` with no argument before it makes what follows the command in the default container) |

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
- **`cmd/flue` embeds the built broker and its sigstore bundle** (`cmd/flue/flue-broker.bin`
  and `cmd/flue/flue-broker.sigstore.json`, go:embed — see `cmd/flue/embed.go`):
  the broker must be built first (the Build/Test commands above do this),
  and the bundle must exist — a release-signed bundle (placed in the
  build context by the release workflow), a locally signed bundle (sign
  `cmd/flue/flue-broker.bin` with `cosign sign-blob --bundle
  cmd/flue/flue-broker.sigstore.json` before compiling), or a copy of the
  committed placeholder template `cmd/flue/flue-broker.sigstore.json.placeholder`
  (the Build/Test commands do this when no bundle is present). If either
  file is missing, compiling `cmd/flue` fails with "no matching files
  found" — a deliberate hard failure. The embedded bytes are stored
  verbatim, so `flue broker executable` / `flue broker sigstore-bundle`
  produce a broker and its bundle 100% identical to the build sources,
  and an extracted broker is verifiable offline with
  `cosign verify-blob --bundle` (the release bundle's identity is the
  release workflow — see the README's verify section). The `.sigstore.json`
  file is gitignored (a build artifact) but *not* containerignored: it
  is a build-context input the Containerfile's build stage consumes.
- **buildah populates the platform builtins**: like BuildKit,
  `buildah` populates `TARGETARCH`, `TARGETPLATFORM`, and
  `BUILDPLATFORM` from the build's platform — the host's platform by
  default, or the `--platform` value when given — and a global-scope
  `ARG` without a default inherits the value (long-standing buildah
  behavior, not BuildKit-only — `TARGETOS` too, which is what the
  golang stages build with). The Containerfile's bare
  `ARG TARGETPLATFORM`/`ARG BUILDPLATFORM`/`ARG TARGETOS`/`ARG
  TARGETARCH` are deliberately default-less: a default in the file
  would override the per-platform value (which is inherited only by
  ARGs without a default) and ship the amd64 binary in the arm64
  image. Don't add defaults, don't remove the bare declarations,
  and don't assume buildah lacks these variables.
- **The Containerfile's stages build for one platform each**: every
  stage compiles for its own `$TARGETOS`/`$TARGETARCH` (a single
  `FROM scratch` `flue-broker` stage, one per platform — never
  resurrect the per-arch stage names or multi-arch loops in a
  stage): buildx evaluates the whole chain once per target platform,
  and buildah compiles for the build's platform. Pass `--platform`
  for a different architecture — never `--build-arg
  TARGETOS/TARGETARCH` overrides: they would pin one platform's
  values onto every platform in a multi-arch build.
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
- **New subcommand**: register it on `rootCmd` in `main.go`'s `init()`
  and assign it a `GroupID` from the root's command groups (Commands,
  Low-level commands, or Utility commands — see the group definitions
  and `TestCobraHelpGroups` in `cli_test.go`): an ungrouped subcommand
  lands in the `--help` output's trailing "Additional Commands" section
  instead of its intended group.
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
