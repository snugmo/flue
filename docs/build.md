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
- Also builds a static `flue` binary in the repo root for local use.
- OCI well-known labels (`org.opencontainers.image.*`, `io.openshift.tags`)
  are passed as `--build-arg`s.

## Containerfile

Two stages:

- **`go-build`**: compiles both binaries with the Go toolchain from the
  cote base image (`GOROOT=/opt/go`). `COPY go.mod go.sum` before
  `RUN go build` gives dependency-level layer caching. Both binaries are
  built `CGO_ENABLED=0` with `-ldflags="-s -w"` (static, stripped — the
  image must run in any Linux container without glibc).
- **final**: copies just the two binaries — the image adds ~2.3 MB over
  the base.

The standalone `BASE_IMAGE` ARG defaults to
`ghcr.io/snugmo/cote:latest-base` when the Containerfile is invoked
without `build.sh`.
