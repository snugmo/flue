# Building the flue image

## Local image build

A plain `buildah` invocation — rootless, no daemon required:

```bash
buildah build --isolation chroot --layers --format docker \
  --build-arg VERSION=<tag> \
  --build-arg REVISION=<sha> \
  --build-arg CREATED=<rfc3339> \
  -t ghcr.io/snugmo/flue:<tag> \
  -f Containerfile .
buildah push ghcr.io/snugmo/flue:<tag>
```

- The tag is the version with the leading `v` stripped (e.g. `0.3.0`),
  so the image ref is `ghcr.io/snugmo/flue:<tag>` (a custom registry
  works the same way — adjust the image reference).
- No platform `--build-arg`s in the command: the Containerfile
  declares the platform builtins without defaults, so each stage
  inherits its own platform's values (a default would pin one
  architecture onto every platform). A plain build produces the
  host's architecture; for another, pass `--platform linux/<arch>`
  (which sets architecture and OS at once) — see AGENTS.md for the
  full rationale.
- `$BUILDPLATFORM` (the build machine's platform) pins the golang
  stages, so the compiler always runs natively regardless of host or
  target architecture; in BuildKit the built-in values keep
  applying.
- `.containerignore` keeps local build artifacts (`flue`,
  `flue-broker`, `cmd/flue/flue-broker.bin`) out of the build context —
  the stages build their own: the embed source is placed by the flue
  build stage from the `flue-broker` stage (one per target platform).
  `cmd/flue/flue-broker.sigstore.json` is deliberately kept in the context:
  it is the build-context input for the broker bundle's `go:embed`
  source (a signed one, when present; else the stage's `cp -n`
  fallback copies the committed placeholder template).
- Building the host-side binaries for local use is the AGENTS.md
  Build command: `flue-broker` to the `go:embed` path, the
  placeholder template as the bundle's embed source when no signed
  bundle is present, `flue`, and a root copy of the broker.
- OCI well-known labels (`org.opencontainers.image.*`,
  `io.openshift.tags`) come from the `--build-arg`s.

## Containerfile

Four stages, each compiling for exactly one target platform — its
own `$TARGETPLATFORM` (a multi-arch buildx build evaluates the whole
chain once per platform):

- **`broker`**: compiles `flue-broker` from source with the
  `golang:${GOVERSION}` toolchain (`ARG GOVERSION`, default `1.27` —
  floating for local builds; the release workflow pins it to the
  `go.mod` `go` directive, so the broker the image compiles is
  byte-identical to the one the release exports and signs),
  `CGO_ENABLED=0` with `-ldflags="-s -w"`
  (static, stripped — the image must run on any Linux container
  without a glibc). Pinned to `FROM --platform=$BUILDPLATFORM` — the
  build machine's platform — and cross-compiles for its own target
  platform (Go cross-compiles natively), so the multi-arch release
  build never needs QEMU: the compiler runs natively on whichever
  host the build runs on.
- **`flue-broker`**: one `FROM scratch` image with the single
  broker binary for the target platform (in a buildx multi-arch
  build, one image per platform) — the single origin of that file:
  the flue build stage consumes the stage file as its `go:embed`
  source (see `cmd/flue/embed.go`), and the release workflow exports,
  signs, and attests the same file, so the attested and signed
  broker is provably the one injected into `flue` at build time.
- **`build`**: compiles `flue` with the same toolchain — pinned to
  `$BUILDPLATFORM` as well — for its own target platform; the stage
  places that platform's broker file on the `go:embed` path
  `cmd/flue/flue-broker.bin` (a flue without its broker is a
  deliberate hard failure), and the broker's sigstore bundle on
  `cmd/flue/flue-broker.sigstore.json` — a signed context copy when
  present, else `cp -n` falls back to the committed placeholder
  template — before compiling.
- **final**: `FROM scratch` — the image is the single static binary at
  `/flue`, with `/flue` as the entrypoint. No base image (hence no
  `org.opencontainers.image.base.*` label) and no other system files:
  a static binary needs no libc, no runtime, no shell. The
  `flue-broker` is not a separate file in the image — it is embedded in
  `flue` (along with its sigstore bundle), recoverable via
  `flue broker executable` and `flue broker sigstore-bundle`.

## Release image (Release workflow)

The release workflow
([`.github/workflows/release.yaml`](../.github/workflows/release.yaml))
builds the image from the same `Containerfile`: one `buildx` build
per architecture, each from its own build-context copy
(`ctx/<arch>`, the repo without `.git`) with that architecture's
signed broker bundle placed at `cmd/flue/flue-broker.sigstore.json`
(a single multi-arch buildx invocation shares one build context, so
per-platform bundles cannot ride one build), then assembles the
`<tag>` and `:latest` manifest list with `docker buildx imagetools
create` (no rebuild, no re-upload).

What is signed and attested, and how to verify it, is in the
README's [Release image](../README.md#release-image) section; the
workflow file is the authority on the exact steps. Two details worth
knowing when touching the workflow: the brokers it signs and attests
are exported from the `flue-broker` stage with `GOVERSION` pinned to
the `go.mod` `go` directive, so they are the exact bytes the
`go:embed` sources consumed (the attested broker is provably the one
injected into `flue`), and the extracted release binaries are
cross-checked byte-for-byte against those exports (`broker
executable` / `broker sigstore-bundle`).
