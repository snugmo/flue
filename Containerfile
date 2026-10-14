# flue — a narrow channel from your host's SSH agent into a container
#
# The flue image, in one multi-stage build:
#
#   broker  a golang toolchain compiles flue-broker, fully static
#           (CGO_ENABLED=0), for both release architectures
#   flue-broker-amd64, flue-broker-arm64
#           one FROM scratch image per architecture, each a single
#           broker binary — the release workflow's attestation input,
#           and the exact file the flue build embeds (see stage
#           comments)
#   build   a golang toolchain compiles flue, fully static, one per
#           architecture, each embedding that architecture's broker
#           file (go:embed)
#   final   FROM scratch: the image is that one static binary at
#           /flue, with /flue as the entrypoint
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

# Platform builtins, declared at global scope with no default: a
# default on any of these would override the per-platform value,
# which both BuildKit and buildah only populate for ARGs without
# one. $BUILDPLATFORM pins the golang stages to the build machine;
# $TARGETARCH selects the binary the final stage ships (buildah
# populates it from the build's platform — the host by default, or
# the --platform value when given; the final stage re-declares it).
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG TARGETARCH

# The broker stage is pinned to the build platform ($BUILDPLATFORM)
# and cross-compiles for the release architectures (Go
# cross-compiles natively with CGO_ENABLED=0), so a multi-arch build
# never needs QEMU: the compiler runs natively on whichever host the
# build runs on — the release workflow's amd64 runner, or an amd64 or
# arm64 dev host.
FROM --platform=$BUILDPLATFORM golang:1.27 AS broker
ARG VERSION=latest
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
# One fully-static broker per release architecture; each is staged in
# its own scratch image below (flue-broker-amd64, flue-broker-arm64),
# which is the single origin of that file everywhere else: the flue
# build stage consumes those stage files as its go:embed sources, and
# the release workflow attests the same files — so the attested
# broker is provably the one injected into flue at build time.
RUN set -eu; for arch in amd64 arm64; do \
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags="-s -w -X main.version=${VERSION}" -o flue-broker_$arch ./cmd/flue-broker; \
  done

# One FROM scratch image per architecture, each a single broker
# binary (mode 0755, as the go build left it). From scratch, so no
# base-image metadata to reconcile; intermediates only — the final
# stage below is the last stage and never becomes an image.
# Absolute --from paths: buildah resolves them from the stage's
# filesystem root, BuildKit from its WORKDIR — both agree on /build.
FROM scratch AS flue-broker-amd64
COPY --chown=0:0 --from=broker /build/flue-broker_amd64 /flue-broker

FROM scratch AS flue-broker-arm64
COPY --chown=0:0 --from=broker /build/flue-broker_arm64 /flue-broker

# The flue build stage is pinned to the build platform
# ($BUILDPLATFORM) and cross-compiles for each architecture (native
# Go cross-compilation), so a multi-arch build never needs QEMU: both
# platforms' flue binaries are compiled natively in this one stage.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG VERSION=latest
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
# The embed sources come from the per-architecture scratch stages
# above, not from this stage's own build: the same files the release
# workflow attests are the ones go:embed pulls into each flue. Both
# hops are deliberate hard failures — cp fails if a stage file is
# missing, and go:embed fails with "no matching files found" if the
# embed path is empty at compile time — so a flue built without its
# broker is never produced silently.
COPY --from=flue-broker-amd64 /flue-broker /build/broker_amd64
COPY --from=flue-broker-arm64 /flue-broker /build/broker_arm64
# Each architecture's flue embeds that architecture's broker: the
# stage file is placed on the go:embed path before compiling it.
RUN set -eu; for arch in amd64 arm64; do \
  cp broker_$arch cmd/flue/flue-broker.bin \
  && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags="-s -w -X main.version=${VERSION}" -o flue_$arch ./cmd/flue; \
  done

# The final stage: FROM scratch, one static binary at the root.
FROM scratch
# Bare ARG (no default): BuildKit populates TARGETARCH per target
# platform (buildah per the build's platform), but a default here
# would override that value for every platform — the arm64 image
# would ship the amd64 binary. To build for a different
# architecture, pass --platform linux/<arch> (never a lone
# --build-arg TARGETARCH=<arch>: it would leave TARGETPLATFORM
# ambiguous); declared globally and re-declared here.
ARG TARGETARCH
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
