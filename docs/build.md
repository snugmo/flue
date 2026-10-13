# Building the flue image

## Local image build

A plain `buildah` invocation — rootless, no daemon required:

```bash
buildah build --isolation chroot --layers --format docker \
  --build-arg BASE_IMAGE=ghcr.io/snugmo/cote:<tag>-base \
  --build-arg VERSION=<tag> --build-arg REVISION=<sha> \
  --build-arg CREATED=<rfc3339> \
  -t ghcr.io/snugmo/flue:<tag> \
  -f Containerfile .
buildah push ghcr.io/snugmo/flue:<tag>
```

- The tag is the version with the leading `v` stripped (e.g. `0.1.0`),
  so the image ref is `ghcr.io/snugmo/flue:<tag>` (a custom registry
  works the same way — adjust both the image and the base reference).
- The base image defaults to `ghcr.io/snugmo/cote:latest-base` (the
  Containerfile's ARG default); the `cote` release at the same tag is
  the usual choice. **The base image must be pushed before building
  flue.**
- `.containerignore` keeps local build artifacts (`flue`, `flue-broker`,
  `cmd/flue/flue-broker.bin`) out of the build context — the go-build
  stage builds its own, and the embed source must be the stage's.
- Building the host-side helpers for local use is the AGENTS.md
  Build command: `flue-broker` first, to the `go:embed` path
  `cmd/flue/flue-broker.bin` (which `flue` embeds), then `flue`, then
  copies that same broker file to the repo root.
- OCI well-known labels (`org.opencontainers.image.*`, `io.openshift.tags`)
  are passed as `--build-arg`s.

## Containerfile

Two stages:

- **`go-build`**: compiles both binaries with the Go toolchain from the
  cote base image (`GOROOT=/opt/go`). It runs as `USER root` because the
  base image's default user is a non-root account that can only write to
  `/tmp`, while the build must place files in root-owned directories
  (`/build/flue/...`, `/usr/local/bin`). `COPY go.mod go.sum` before
  `RUN go build` gives dependency-level layer caching. Both binaries are
  built `CGO_ENABLED=0` with `-ldflags="-s -w"` (static, stripped — the
  image must run in any Linux container without glibc). The broker is
  built first, to the `go:embed` path `cmd/flue/flue-broker.bin` (flue
  embeds it — see `cmd/flue/embed.go`), then `flue`, and the *same file*
  is copied to `/usr/local/bin/flue-broker`: so a `flue` from this image
  extracts (via `flue extract-broker`) a broker byte-identical to the
  image's own.
- **final**: copies just the two binaries (~19 MB uncompressed; `flue`
  now carries the embedded broker, so it is larger than before) and keeps
  the base image's non-root user, so the shipped container runs
  unprivileged.

The standalone `BASE_IMAGE` ARG defaults to
`ghcr.io/snugmo/cote:latest-base` when the Containerfile is built
directly.

## Release image (Release workflow)

`Containerfile.release` is the release path: a single stage that
`COPY`s the two pre-built binaries (no `go-build` stage, no
compilation, no QEMU — the final stage runs nothing). The Release
workflow's `image` job places the binaries the `build` job built and
attested into the context as
`dist/<arch>/{flue,flue-broker}`, and BuildKit's auto-populated
`ARG TARGETARCH` selects each platform's pair at `COPY` time.
`--chown=root:root` pins the ownership, because a context `COPY` would
otherwise keep the runner user's uid; the final stage keeps the base
image's non-root user, as the dev image does.

The image job then:

- pushes `ghcr.io/snugmo/flue:<tag>` and `:latest` (buildx,
  `linux/amd64,linux/arm64`);
- attests the image with `actions/attest` (SLSA build provenance, the
  same toolkit as the binary attestations): stored in GitHub's
  attestation store, pushed to the registry as a cosign attestation,
  and registered as an artifact storage record;
- signs it with a Sigstore keyless cosign signature (one signature per
  manifest-list digest, so all tags are covered).

The base image is the `cote` release at the same tag, which must be
published (multi-arch) before the job runs, and can be overridden
with the `FLUE_BASE_IMAGE` repository variable.

The dev path (buildah + `Containerfile`) compiles from source; release
images ship the attested bytes.
