#!/usr/bin/env bash
# Build and optionally push the flue image.
# Usage:
#   ./build.sh                          # Build only
#   ./build.sh push                     # Build and push
#   ./build.sh push <registry>          # Build and push to custom registry
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}"

usage() {
    printf 'Usage:\n'
    printf '  ./build.sh                          # Build only\n'
    printf '  ./build.sh push                     # Build and push\n'
    printf '  ./build.sh push <registry>          # Build and push to custom registry\n'
}

# Configurable via environment or defaults
REGISTRY="${DEV_REGISTRY:-ghcr.io/snugmo}"
IMAGE_NAME="${DEV_IMAGE_NAME:-flue}"
VERSION="${DEV_VERSION:-$(git -C "$PROJECT_ROOT" describe --tags --always --dirty 2>/dev/null || echo 'latest')}"
# Values for the org.opencontainers.image.* well-known labels (see Containerfile)
REVISION="$(git -C "$PROJECT_ROOT" rev-parse HEAD 2>/dev/null || echo 'unknown')"
CREATED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TAG="${VERSION#v}"

PUSH=false
if [[ $# -gt 2 ]]; then
    printf '\nError: Too many arguments. '
    usage
    printf '\nAborting.\n'
    exit 2
fi

if [[ "${1:-}" == "push" ]]; then
    PUSH=true
    if [[ -n "${2:-}" ]]; then
        REGISTRY="$2"
    fi
elif [[ -n "${1:-}" ]]; then
    printf '\n⚠ WARNING: First argument "%s" is not "push".\n' "$1"
    printf '   Image will be built locally but NOT pushed.\n'
    printf '   Did you mean:  ./build.sh push %s\n\n' "$1"
    usage
    printf '\nAborting.\n'
    exit 2
fi

# BASE_IMAGE is computed here, after arg parsing, so a registry passed as the
# second argument applies to the base image reference as well.
BASE_IMAGE="${DEV_BASE_IMAGE:-${REGISTRY}/cote:${TAG}-base}"

FULL_IMAGE="${REGISTRY}/${IMAGE_NAME}:${TAG}"

printf '=== flue Image ===\n'
printf 'Registry:  %s\n' "${REGISTRY}"
printf 'Image:     %s\n' "${IMAGE_NAME}"
printf 'Tag:       %s\n' "${TAG}"
printf 'Full ref:  %s\n' "${FULL_IMAGE}"
printf 'Base:      %s\n' "${BASE_IMAGE}"
printf '\n'

# Build
printf '>> Building %s ...\n' "${FULL_IMAGE}"
buildah build \
    --isolation chroot \
    --layers \
    --format docker \
    --storage-opt ignore_chown_errors=true \
    --build-arg BASE_IMAGE="${BASE_IMAGE}" \
    --build-arg VERSION="${TAG}" \
    --build-arg REVISION="${REVISION}" \
    --build-arg CREATED="${CREATED}" \
    -t "${FULL_IMAGE}" \
    -f "${SCRIPT_DIR}/Containerfile" \
    "${SCRIPT_DIR}"

# Also build the host-side helper binary for local use.
printf '>> Building fluehost (host-side helper) ...\n'
CGO_ENABLED=0 go build -ldflags="-s -w" -o "${PROJECT_ROOT}/fluehost" "${PROJECT_ROOT}/cmd/fluehost"

printf '>> Build complete: %s\n' "${FULL_IMAGE}"

# Push (optional)
if $PUSH; then
    printf '\n'
    printf '>> Pushing %s ...\n' "${FULL_IMAGE}"
    buildah push "${FULL_IMAGE}"
    printf '>> Push complete.\n'
fi

printf '\n'
printf '>> Done. To use in a Devfile, reference:\n'
printf '    image: %s\n' "${FULL_IMAGE}"
