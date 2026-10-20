# flue — a narrow channel from your host's SSH agent into a container
#
# The flue image, in one multi-stage build:
#
#   broker    a golang toolchain compiles flue-broker, fully static
#             (CGO_ENABLED=0), for the target platform
#   flue-broker
#             one FROM scratch image with the single broker binary
#             — the release workflow's attestation input, and the
#             exact file the flue build embeds (see stage comments)
#   build     a golang toolchain compiles flue, fully static, for the
#             target platform, embedding that platform's broker file
#             and the broker's sigstore bundle (go:embed; the bundle
#             is a build-context input — release-signed, locally
#             signed, or the committed placeholder fallback)
#   final     FROM scratch: the image is that one static binary at
#             /flue, with /flue as the entrypoint
#
# Every stage compiles for exactly one target platform — its own
# $TARGETOS/$TARGETARCH. A multi-arch build (buildx) therefore
# evaluates this same chain once per platform; a local buildah
# build compiles for the build's platform (host default, or
# --platform). The golang stages use the golang:${GOVERSION}
# toolchain (ARG GOVERSION, default 1.27 — floating for local
# builds; the release workflow pins it to the go.mod `go` directive,
# so the broker bytes are identical across every buildx invocation
# in one release — including the broker the release signs and
# embeds, see below).
#
# The final stage has no base image and no other system files: a
# static binary needs no libc, no runtime, no shell. The release
# workflow's image job and the local build below both build this one
# file, so the image is built the same way in both.
#
# Build (local dev — the README Build section has the full form):
#   buildah build --isolation chroot --layers --format docker \
#     --build-arg VERSION=<tag> \
#     -t <image> -f Containerfile .
#
# The build stage's sigstore-bundle embed source (the flue broker
# sigstore-bundle subcommand's file, cmd/flue/flue-broker.sigstore.json)
# comes from the build context, not from a stage: a signed
# cmd/flue/flue-broker.sigstore.json in the context wins (the release
# workflow places the platform's cosign-signed bundle there; a local
# user may sign their own broker first), else the committed placeholder
# template (cmd/flue/flue-broker.sigstore.json.placeholder) is copied over —
# so a bare local build succeeds with the labeled placeholder, and an
# unsigned build is never confused with a signed one (see
# cmd/flue/embed.go).
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
# one. GOVERSION is a regular (non-platform) arg with a default:
# floating for local builds, pinned by the release workflow.
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG GOVERSION=1.27

# The broker stage is pinned to the build platform (${BUILDPLATFORM})
# and cross-compiles for its own target platform (Go cross-compiles
# natively with CGO_ENABLED=0), so a multi-arch build never needs
# QEMU: the compiler runs natively on whichever host the build runs
# on — the release workflow's amd64 runner, or an amd64 or arm64
# dev host.
FROM --platform=${BUILDPLATFORM} golang:${GOVERSION} AS broker
ARG VERSION=latest
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
# One fully static broker for this build's target platform (GOOS
# and GOARCH from the $TARGETOS/$TARGETARCH builtins); it is staged
# in the scratch image below, which is the single origin of that
# file everywhere else: the flue build stage consumes the stage
# file as its go:embed source, and the release workflow exports,
# attests, and cosign-signs the same file (the signature is
# embedded in flue as the broker's sigstore bundle) — so the
# attested and signed broker is provably the one injected into
# flue at build time.
RUN set -eu; CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -ldflags="-s -w -X main.version=${VERSION}" -o flue-broker ./cmd/flue-broker

# One FROM scratch image with the target platform's single broker
# binary (mode 0755, as the go build left it). In a buildx
# multi-arch build this is one image per platform: each platform's
# chain compiles its own broker here. From scratch, so no
# base-image metadata to reconcile; intermediates only — the final
# stage below is the last stage and never becomes an image.
# Absolute --from paths: buildah resolves them from the stage's
# filesystem root, BuildKit from its WORKDIR — both agree on /build.
FROM scratch AS flue-broker
COPY --chown=0:0 --from=broker /build/flue-broker /flue-broker

# The flue build stage is pinned to the build platform
# (${BUILDPLATFORM}) and cross-compiles for its own target platform
# (native Go cross-compilation), so a multi-arch build never needs
# QEMU: each platform's flue binary is compiled natively by that
# platform's instance of this stage.
FROM --platform=${BUILDPLATFORM} golang:${GOVERSION} AS build
ARG VERSION=latest
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
# The embed source comes from the flue-broker stage above, not from
# this stage's own build: the same file the release workflow attests
# is the one go:embed pulls into flue. Both hops are deliberate hard
# failures — the COPY fails if the stage file is missing, and
# go:embed fails with "no matching files found" if the embed path is
# empty at compile time — so a flue built without its broker is
# never produced silently. The destination is placed relative to
# WORKDIR (both builders agree on WORKDIR for destinations), onto
# the go:embed path.
COPY --from=flue-broker /flue-broker cmd/flue/flue-broker.bin
# The broker's sigstore bundle (the second go:embed source, see
# cmd/flue/embed.go): the COPY of cmd/ above already brought in the
# context's cmd/flue/flue-broker.sigstore.json when one is present (the
# release workflow places the platform's cosign-signed bundle in each
# platform's build context; a local user may sign their own broker
# first) — and the committed placeholder template, which this cp -n
# copies to the embed path only when no signed bundle is present.
# So a signed context always wins over the placeholder, a bare local
# build falls back to it, and go:embed never sees a missing file.
RUN cp -n cmd/flue/flue-broker.sigstore.json.placeholder cmd/flue/flue-broker.sigstore.json
RUN set -eu; CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -ldflags="-s -w -X main.version=${VERSION}" -o flue ./cmd/flue

# The final stage: FROM scratch, one static binary at the root. It
# ships the target platform's flue, as built by the build stage
# above — in buildx each platform's final stage instance pulls from
# the same platform's build stage instance, and in buildah the
# build's platform (host default, or --platform) is the one.
FROM scratch
# Stamped into the OCI well-known labels below (the binary already
# carries the version, stamped by the build stage).
ARG VERSION=latest
ARG REVISION=unknown
ARG CREATED=
# --chown=0:0 (numeric, not "root":root) because scratch has no
# /etc/passwd to resolve a name; 0:0 is the only meaningful
# ownership, and with no USER instruction the process runs as uid 0
# anyway.
# Absolute path: buildah resolves --from sources from the stage's
# filesystem root, while BuildKit uses the stage's WORKDIR; both
# agree on an absolute one.
COPY --chown=0:0 --from=build /build/flue /flue

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
