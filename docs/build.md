# Building the flue image

## build.sh

- Uses `buildah` (not docker) — rootless, no daemon required, works in CI.
- The tag is the git-describe version (`git describe --tags --always
  --dirty`, leading `v` stripped, falling back to `latest`), so the image
  ref is `ghcr.io/snugmo/flue:<tag>` — e.g. `0.1.0`.
- `./build.sh` builds only; `./build.sh push` pushes to `ghcr.io/snugmo`;
  `./build.sh push <registry>` pushes to a custom registry (the registry
  also applies to the base-image reference).
- Overrides: `DEV_VERSION=`, `DEV_IMAGE_NAME=`, `DEV_REGISTRY=` (use
  `localhost:5000` for local dev / CI), `DEV_BASE_IMAGE=`.
- The base image defaults to `${REGISTRY}/cote:${TAG}-base` (the cote
  base image at the same git-describe tag). **The base image must be
  pushed before building flue**; override with `DEV_BASE_IMAGE`.
- `.containerignore` keeps local build artifacts (`flue`, `flue-broker`,
  `cmd/flue/flue-broker.bin`) out of the build context — the go-build
  stage builds its own, and the embed source must be the stage's.
- Also builds the host-side helpers for local use: `flue-broker` first,
  to the `go:embed` path `cmd/flue/flue-broker.bin` (which `flue` embeds),
  then `flue`, then copies that same broker file to the repo root.
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
`ghcr.io/snugmo/cote:latest-base` when the Containerfile is invoked
without `build.sh`.
