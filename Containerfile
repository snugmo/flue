# flue — a narrow channel from your host's SSH agent into a container
#
# Builds flue (container-side) and fluehost (host-side): a filtered SSH
# agent bridge. flue forwards client connection fds to the host over a
# socketpair control channel; fluehost relays each to the real agent with
# key-allowlist filtering.
#
# Build:
#   ./build.sh                          # Build only
#   ./build.sh push <registry>          # Build and push
#
# Usage:
#   fluehost -- podman exec \
#     --preserve-fds 1 \
#     -e SSH_AUTH_SOCK=/tmp/flue.sock \
#     <container> flue

ARG BASE_IMAGE=ghcr.io/snugmo/cote:latest-base
FROM ${BASE_IMAGE} AS go-build
ENV GOPATH=/go \
    GOROOT=/opt/go \
    PATH=/go/bin:/opt/go/bin:$PATH

WORKDIR /build/flue

COPY go.mod go.sum ./
COPY cmd/ cmd/

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/flue ./cmd/flue \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/fluehost ./cmd/fluehost

FROM ${BASE_IMAGE}

COPY --from=go-build /usr/local/bin/flue /usr/local/bin/flue
COPY --from=go-build /usr/local/bin/fluehost /usr/local/bin/fluehost

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
