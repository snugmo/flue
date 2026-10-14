# flue — a narrow channel from your host's SSH agent into a container
#
# The flue image, in one multi-stage build:
#
#   build   a golang toolchain compiles flue, fully static
#           (CGO_ENABLED=0), embedding the flue-broker built from the
#           same source (go:embed, built first, to the embed path)
#   final   FROM scratch: the image is that one static binary at /flue,
#           with /flue as the entrypoint
#
# The final stage has no base image and no other system files: a static
# binary needs no libc, no runtime, no shell. The release workflow's
# image job and the local build below both build this one file, so the
# image is built the same way in both.
#
# Build (local dev — the README Build section has the full form):
#   buildah build --isolation chroot --layers --format docker \
#     --build-arg VERSION=<tag> \
#     -t <image> -f Containerfile .
#
# Usage (the entrypoint is /flue, so the subcommand and its arguments
# go straight after the image):
#   podman run --rm <image> version
#   podman run --rm <image> \
#     exec -- podman exec \
#     --preserve-fds 1 \
#     -e SSH_AUTH_SOCK=/tmp/flue.sock \
#     <target> flue-broker -sock /tmp/flue.sock

# The build stage is pinned to linux/amd64 and cross-compiles for the
# target architecture (Go cross-compiles natively with CGO_ENABLED=0),
# so a multi-arch build never needs QEMU: on the release workflow's
# amd64 runner — and any amd64 dev host — both platforms' binaries are
# compiled natively.
FROM --platform=linux/amd64 golang:1.26 AS build
ARG VERSION=latest
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
# The broker is built first, to the go:embed path, because flue embeds
# it (compiling flue without it fails — a deliberate hard failure).
# It is rebuilt before each architecture's flue, so each flue embeds
# the broker built for the same architecture; both release
# architectures are compiled in this one native-amd64 stage.
RUN set -eu; for arch in amd64 arm64; do \
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags="-s -w -X main.version=${VERSION}" -o cmd/flue/flue-broker.bin ./cmd/flue-broker \
  && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags="-s -w -X main.version=${VERSION}" -o flue_$arch ./cmd/flue; \
  done

# The final stage: FROM scratch, one static binary at the root.
FROM scratch
# BuildKit populates TARGETARCH per build platform, so a multi-arch
# build copies the matching binary. buildah does not know the variable:
# the default (amd64) matches a local build on an amd64 host; pass
# --build-arg TARGETARCH=arm64 for an arm64 image.
ARG TARGETARCH=amd64
# Stamped into the OCI well-known labels below (the binary already
# carries the version, stamped by the build stage).
ARG VERSION=latest
ARG REVISION=unknown
ARG CREATED=
# --chown=0:0 (numeric, not "root":root) because scratch has no
# /etc/passwd to resolve a name; 0:0 is the only meaningful ownership,
# and with no USER instruction the process runs as uid 0 anyway.
# Absolute path: buildah resolves --from sources from the stage's
# filesystem root, while BuildKit uses the stage's WORKDIR; both agree
# on an absolute one.
COPY --chown=0:0 --from=build /build/flue_${TARGETARCH} /flue

# The binary is the whole image.
ENTRYPOINT ["/flue"]

# No base image, so no org.opencontainers.image.base.* label.
LABEL org.opencontainers.image.title="flue" \
      org.opencontainers.image.description="Filtered SSH agent bridge from host to container via SCM_RIGHTS control channel" \
      org.opencontainers.image.authors="Joakim Nohlgård" \
      org.opencontainers.image.vendor="Snugmo" \
      org.opencontainers.image.licenses="AGPL-3.0" \
      org.opencontainers.image.url="https://github.com/snugmo/flue" \
      org.opencontainers.image.documentation="https://github.com/snugmo/flue" \
      org.opencontainers.image.source="https://github.com/snugmo/flue.git" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      io.openshift.tags="dev,ssh,agent,proxy"
