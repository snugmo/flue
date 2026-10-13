# flue — a narrow channel from your host's SSH agent into a container
#
# Builds flue-broker (container-side) and flue (host-side): a filtered SSH
# agent bridge. flue-broker listens on a Unix socket inside the container
# and hands that listening fd to the host over a socketpair control
# channel; flue accepts client connections on it and proxies each to the
# real agent with key-allowlist filtering.
#
# Build:
#   ./build.sh                          # Build only
#   ./build.sh push <registry>          # Build and push
#
# Usage:
#   flue exec -- podman exec \
#     --preserve-fds 1 \
#     -e SSH_AUTH_SOCK=/tmp/flue.sock \
#     <container> flue-broker -sock /tmp/flue.sock

ARG BASE_IMAGE=ghcr.io/snugmo/cote:latest-base
FROM ${BASE_IMAGE} AS go-build
# Stamped into both binaries with -ldflags "-X main.version=..." (shown by
# `flue version` / `flue-broker -version`); the final stage re-declares it
# for the image labels.
ARG VERSION=latest
ENV GOPATH=/go \
    GOROOT=/opt/go \
    PATH=/go/bin:/opt/go/bin:$PATH

# The cote base image's default user is a non-root account that can only
# write to /tmp; the build must place files in root-owned directories
# (/build/flue/..., /usr/local/bin), so the build stage runs as root.
# The final stage keeps the base image's non-root user, so the shipped
# container still runs unprivileged.
USER root

WORKDIR /build/flue

COPY go.mod go.sum ./
COPY cmd/ cmd/

# flue embeds the built broker (go:embed; see cmd/flue/embed.go), so the
# broker is built first, to the embed path. The *same file* is then copied
# to /usr/local/bin/flue-broker, so the image's flue extract-broker output
# is byte-identical to the image's own flue-broker.
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /build/flue/cmd/flue/flue-broker.bin ./cmd/flue-broker \
 && CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /usr/local/bin/flue ./cmd/flue \
 && cp /build/flue/cmd/flue/flue-broker.bin /usr/local/bin/flue-broker

FROM ${BASE_IMAGE}

COPY --from=go-build /usr/local/bin/flue-broker /usr/local/bin/flue-broker
COPY --from=go-build /usr/local/bin/flue /usr/local/bin/flue

ARG VERSION=latest
ARG REVISION=unknown
ARG CREATED=
ARG BASE_IMAGE=ghcr.io/snugmo/cote:latest-base

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
      org.opencontainers.image.base.name="${BASE_IMAGE}" \
      io.openshift.tags="dev,ssh,agent,proxy"
