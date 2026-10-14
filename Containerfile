# flue — a narrow channel from your host's SSH agent into a container
#
# The flue image, in one multi-stage build:
#
#   build   a golang toolchain compiles flue, fully static
#           (CGO_ENABLED=0), embedding the flue-broker built from the
#           same source (go:embed, built first, to the embed path)
#   brokers FROM scratch: the two flue-broker binaries from the build
#           stage — an intermediate only, not part of the image; the
#           release workflow exports it to attest the brokers for
#           every architecture (see its stage comment)
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
FROM --platform=linux/amd64 golang:1.27 AS build
ARG VERSION=latest
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
# The broker is built first, to the go:embed path, because flue embeds
# it (compiling flue without it fails — a deliberate hard failure).
# It is rebuilt before each architecture's flue, so each flue embeds
# the broker built for the same architecture; both release
# architectures are compiled in this one native-amd64 stage. Each
# broker is also copied to flue-broker_<arch> — the per-architecture
# copies feed the brokers stage below.
RUN set -eu; for arch in amd64 arm64; do \
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags="-s -w -X main.version=${VERSION}" -o cmd/flue/flue-broker.bin ./cmd/flue-broker \
  && cp cmd/flue/flue-broker.bin flue-broker_$arch \
  && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags="-s -w -X main.version=${VERSION}" -o flue_$arch ./cmd/flue; \
  done

# The brokers stage: the two per-architecture flue-broker binaries.
# It is an intermediate (the final stage below is the last stage, so
# this one never becomes the image), and it exists so the release
# workflow can attest the broker for every architecture without
# running any non-native binary: the files are the exact ones the
# build stage's flue binaries embedded — the loop copies each to
# flue-broker_<arch> right after building it, before compiling that
# architecture's flue — so the workflow exports this stage
# (docker buildx build --target brokers --output type=local), and a
# native extract-broker run on the pushed image cross-checks the
# pairing byte-for-byte.
FROM scratch AS brokers
# Absolute paths: buildah resolves --from sources from the stage's
# filesystem root, BuildKit from its WORKDIR — both agree on /build
# (the final stage's COPY below relies on the same).
COPY --chown=0:0 --from=build /build/flue-broker_amd64 /flue-broker_amd64
COPY --chown=0:0 --from=build /build/flue-broker_arm64 /flue-broker_arm64

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
