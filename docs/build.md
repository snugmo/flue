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

- The tag is the version with the leading `v` stripped (e.g. `0.2.0`),
  so the image ref is `ghcr.io/snugmo/flue:<tag>` (a custom registry
  works the same way — adjust the image reference).
- No platform `--build-arg`s in the command: the Containerfile
  declares the platform builtins (`TARGETPLATFORM`, `BUILDPLATFORM`,
  `TARGETOS`, `TARGETARCH`) at global scope with no default, so the
  builder's per-platform values are inherited (a default in the
  file would override them and ship the amd64 binary in the arm64
  image). buildah populates them from the build's platform — the
  host by default, or the `--platform` value when given — like
  BuildKit, so a plain build produces the host's architecture. To
  build for a different architecture, pass `--platform linux/<arch>`
  (e.g. `--platform linux/arm64`): it sets the whole target
  platform (architecture and OS) at once, whereas
  `--build-arg TARGETARCH=<arch>` (or `TARGETOS=`) would pin that
  one value onto every platform.
- `$BUILDPLATFORM` (the build machine's platform) pins the golang
  stages, so the compiler always runs natively regardless of host or
  target architecture; in BuildKit the built-in values keep
  applying.
- `.containerignore` keeps local build artifacts (`flue`,
  `flue-broker`, `cmd/flue/flue-broker.bin`) out of the build context —
  the stages build their own: the embed source is placed by the flue
  build stage from the `flue-broker` stage (one per target platform).
  `cmd/flue/flue-broker.sigstore` is deliberately kept in the context:
  it is the build-context input for the broker bundle's `go:embed`
  source (a signed one, when present; else the stage's `cp -n`
  fallback copies the committed placeholder template).
- Building the host-side helpers for local use is the AGENTS.md
  Build command: `flue-broker` first, to the `go:embed` path
  `cmd/flue/flue-broker.bin` (which `flue` embeds); then, when no
  signed `cmd/flue/flue-broker.sigstore` is present, a copy of the
  committed placeholder template to that path (the bundle's second
  `go:embed` source); then `flue`; then a copy of that same broker
  file to the repo root.
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
  `cmd/flue/flue-broker.sigstore` — a signed context copy when
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

The release image is built from the same `Containerfile` — one
`buildx` build per architecture, each from its own copy of the build
context (`ctx/<arch>`, the repo without `.git`) with that
architecture's signed broker bundle placed at
`cmd/flue/flue-broker.sigstore`. A single multi-arch buildx
invocation shares one build context across its platforms, so the
per-platform signed bundles cannot ride one build; the two
single-platform images are then assembled into the `<tag>` and
`:latest` manifest list with `docker buildx imagetools create` (no
rebuild, no re-upload). BuildKit evaluates the chain once per target
platform; the golang stages, pinned to `$BUILDPLATFORM`, each
cross-compile their own platform natively — no QEMU.

The `image` job:

- exports the broker from the `flue-broker` stage per architecture
  (`buildx --target` builds with a per-architecture `--platform` and
  a local output; `GOVERSION` pinned to the `go.mod` `go` directive
  and the same `VERSION` as the image, so the exported bytes are the
  image's bytes);
- signs each exported broker with `cosign sign-blob --keyless` — the
  bundle's certificate identity is the release workflow, so the
  signed pair proves the broker was built by this workflow;
- builds and pushes `ghcr.io/snugmo/flue:<tag>-<arch>` per
  architecture (each image embedding that architecture's signed
  bundle), then assembles the `<tag>` and `:latest` manifest list;
- attests each per-architecture image with an SLSA
  build-provenance attestation (the predicate is generated from the
  workflow OIDC claims — repo, ref, builder — so per-architecture
  attestations are valid), and signs the manifest list with a
  Sigstore keyless cosign signature (one signature per digest, so
  all tags are covered);
- extracts the release binaries from the pushed image by the
  manifest-list digest and copies the `/flue` file out — a
  downloaded binary is byte-identical to the one that was pushed,
  signed, and attested — and cross-checks both embedded pairs
  byte-for-byte: a `broker executable` run against the exported
  broker, and a `broker sigstore-bundle` run against the signed
  bundle;
- attests the two `flue-broker` files (a `subject-path` attestation
  each) — the same files the flue build stage's `COPY --from`
  consumed as the `go:embed` sources, so the attested broker is the
  one injected at build time.
