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
  declares `TARGETARCH`, `TARGETPLATFORM`, and `BUILDPLATFORM` at
  global scope with no default, so the builder's per-platform values
  are inherited (a default in the file would override them and ship
  the amd64 binary in the arm64 image). buildah populates them from
  the build's platform — the host by default, or the `--platform`
  value when given — like BuildKit, so a plain build produces the
  host's architecture. To build for a different architecture, pass
  `--platform linux/<arch>` (e.g. `--platform linux/arm64`): it sets
  the whole target platform (architecture and OS), whereas a lone
  `--build-arg TARGETARCH=<arch>` would leave `TARGETPLATFORM`
  ambiguous.
- `$BUILDPLATFORM` (the build machine's platform) pins the golang
  stages, so the compiler always runs natively regardless of host or
  target architecture; in BuildKit the built-in values keep
  applying.
- `.containerignore` keeps local build artifacts (`flue`,
  `flue-broker`, `cmd/flue/flue-broker.bin`) out of the build context —
  the stages build their own: the embed source is placed by the flue
  build stage from the per-architecture broker stages.
- Building the host-side helpers for local use is the AGENTS.md
  Build command: `flue-broker` first, to the `go:embed` path
  `cmd/flue/flue-broker.bin` (which `flue` embeds), then `flue`, then
  copies that same broker file to the repo root.
- OCI well-known labels (`org.opencontainers.image.*`,
  `io.openshift.tags`) come from the `--build-arg`s.

## Containerfile

Five stages:

- **`broker`**: compiles `flue-broker` from source with the
  `golang:1.27` toolchain, `CGO_ENABLED=0` with `-ldflags="-s -w"`
  (static, stripped — the image must run on any Linux container
  without a glibc). Pinned to `FROM --platform=$BUILDPLATFORM` — the
  build machine's platform — and cross-compiles both release
  architectures in one loop (Go cross-compiles natively), so the
  multi-arch release build never needs QEMU: the compiler runs
  natively on whichever host the build runs on.
- **`flue-broker-amd64`** and **`flue-broker-arm64`**: one
  `FROM scratch` image per architecture, each a single broker binary
  — the single origin of that file: the flue build stage consumes the
  stage files as its `go:embed` sources (see `cmd/flue/embed.go`),
  and the release workflow attests the same files, so the attested
  broker is provably the one injected into `flue` at build time.
- **`build`**: compiles `flue` with the same toolchain — pinned to
  `$BUILDPLATFORM` as well — one binary per architecture; each
  `flue` embeds that architecture's broker file, which the stage
  places on the `go:embed` path `cmd/flue/flue-broker.bin` before
  compiling (a flue without its broker is a deliberate hard failure).
- **final**: `FROM scratch` — the image is the single static binary at
  `/flue`, with `/flue` as the entrypoint. No base image (hence no
  `org.opencontainers.image.base.*` label) and no other system files:
  a static binary needs no libc, no runtime, no shell. The
  `flue-broker` is not a separate file in the image — it is embedded in
  `flue`, recoverable via `flue extract-broker`.

## Release image (Release workflow)

The release image is built from the same `Containerfile`: the
workflow's `image` job runs a `buildx` build for `linux/amd64` and
`linux/arm64` (BuildKit populates `TARGETARCH` per platform; both
platforms are compiled natively in the build stage pinned to
`$BUILDPLATFORM` — no QEMU), tagging `ghcr.io/snugmo/flue:<tag>`
and `:latest` from the tagged source.

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
attested for both architectures: the job exports the
`flue-broker-<arch>` stages of the same build (fully cached `buildx`
invocations with `--target` and a local output) and attests each
file — the same files the flue build stage's `COPY --from` consumed
as the `go:embed` sources, so the attested broker is the one
injected at build time — and a native `extract-broker` run on the
pushed image's `flue` cross-checks the pairing byte-for-byte.
