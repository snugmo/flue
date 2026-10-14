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
- `TARGETARCH` (which architecture's `flue` the final stage ships)
  defaults to `amd64` — `buildah` does not populate the BuildKit
  variable — which matches a build on an amd64 host; pass
  `--build-arg TARGETARCH=arm64` for an arm64 image.
- `.containerignore` keeps local build artifacts (`flue`,
  `flue-broker`, `cmd/flue/flue-broker.bin`) out of the build context —
  the build stage builds its own, and the embed source must be the
  stage's.
- Building the host-side helpers for local use is the AGENTS.md
  Build command: `flue-broker` first, to the `go:embed` path
  `cmd/flue/flue-broker.bin` (which `flue` embeds), then `flue`, then
  copies that same broker file to the repo root.
- OCI well-known labels (`org.opencontainers.image.*`,
  `io.openshift.tags`) come from the `--build-arg`s.

## Containerfile

Two stages:

- **`build`**: compiles both binaries from source with the
  `golang:1.26` toolchain, `CGO_ENABLED=0` with `-ldflags="-s -w"`
  (static, stripped — the image must run on any Linux container without
  a glibc). It is pinned to `FROM --platform=linux/amd64` and
  cross-compiles both release architectures in one loop (Go
  cross-compiles natively), so the multi-arch release build never needs
  QEMU. For each architecture the broker is built first, to the
  `go:embed` path `cmd/flue/flue-broker.bin` (flue embeds it — see
  `cmd/flue/embed.go`), then `flue` for that architecture, so each
  `flue` embeds the broker built for the same architecture.
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
platforms are compiled natively in the pinned amd64 build stage — no
QEMU), tagging `ghcr.io/snugmo/flue:<tag>` and `:latest` from the
tagged source.

The image job then:

- attests the manifest list with an SLSA build-provenance attestation
  (the attest step's `provenance: true` generates the predicate for
  the buildx run — the same toolkit as the binary attestations in the
  build job): stored in GitHub's attestation store, pushed to the
  registry as a cosign attestation, and registered as an artifact
  storage record;
- signs it with a Sigstore keyless cosign signature (one signature per
  manifest-list digest, so all tags are covered).

The binaries published as release assets are built and attested by the
`build` job from the same commit with the same build flags; the
image's build stage compiles that same source itself.
