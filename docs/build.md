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

- The tag is the version with the leading `v` stripped (e.g. `0.1.0`),
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
- Building the host-side helpers for local use is the AGENTS.md
  Build command: `flue-broker` first, to the `go:embed` path
  `cmd/flue/flue-broker.bin` (which `flue` embeds), then `flue`, then
  copies that same broker file to the repo root.
- OCI well-known labels (`org.opencontainers.image.*`,
  `io.openshift.tags`) come from the `--build-arg`s.

## Containerfile

Four stages, each compiling for exactly one target platform — its
own `$TARGETPLATFORM` (a multi-arch buildx build evaluates the whole
chain once per platform):

- **`broker`**: compiles `flue-broker` from source with the
  `golang:1.27` toolchain, `CGO_ENABLED=0` with `-ldflags="-s -w"`
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
  source (see `cmd/flue/embed.go`), and the release workflow attests
  the same file, so the attested broker is provably the one
  injected into `flue` at build time.
- **`build`**: compiles `flue` with the same toolchain — pinned to
  `$BUILDPLATFORM` as well — for its own target platform; the stage
  places that platform's broker file on the `go:embed` path
  `cmd/flue/flue-broker.bin` before compiling (a flue without its
  broker is a deliberate hard failure).
- **final**: `FROM scratch` — the image is the single static binary at
  `/flue`, with `/flue` as the entrypoint. No base image (hence no
  `org.opencontainers.image.base.*` label) and no other system files:
  a static binary needs no libc, no runtime, no shell. The
  `flue-broker` is not a separate file in the image — it is embedded in
  `flue`, recoverable via `flue extract-broker`.

## Release image (Release workflow)

The release image is built from the same `Containerfile`: the
workflow's `image` job runs a `buildx` build for `linux/amd64` and
`linux/arm64` (BuildKit evaluates the chain once per target
platform; the golang stages, pinned to `$BUILDPLATFORM`, each
cross-compile their own platform natively — no QEMU), tagging
`ghcr.io/snugmo/flue:<tag>` and `:latest` from the tagged source.

The image job then:

- attests the manifest list with an SLSA build-provenance attestation
  (the attest step's `provenance: true` generates the predicate for
  the buildx run — the same toolkit as the binary attestations in the
  build job): stored in GitHub's attestation store, pushed to the
  registry as a cosign attestation, and registered as an artifact
  storage record;
- signs it with a Sigstore keyless cosign signature (one signature per
  manifest-list digest, so all tags are covered).

The binaries published as release assets are the image's own bytes: the
`image` job pulls each architecture's manifest from the registry by the
manifest-list digest the build step just pushed and copies the `/flue`
file out, so a downloaded binary is byte-identical to the one that was
pushed, signed, and attested. The two `flue-broker` binaries (not
release assets — each ships inside its architecture's `flue`) are
attested for both architectures: the job builds the `flue-broker`
stage of the same build once per architecture (fully cached `buildx`
invocations with `--target`, a per-architecture `--platform`, and a
local output) and attests each file — the same files the flue build
stage's `COPY --from` consumed as the `go:embed` sources, so the
attested broker is the one injected at build time — and a native
`extract-broker` run on the pushed image's `flue` cross-checks the
pairing byte-for-byte.
