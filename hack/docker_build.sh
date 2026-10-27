#!/usr/bin/env bash
# Build everything in a Go 1.26 Docker container and copy the artifacts to ./dist.
#
#   hack/docker_build.sh                       version from git describe (else "dev"), noaudio Linux builds
#   VERSION=1.2.3 hack/docker_build.sh
#   NOAUDIO=0 hack/docker_build.sh             link ALSA local audio into the Linux binaries
#   APPS=goslimplayer TARGETS="linux/amd64" hack/docker_build.sh
#   IMAGE=my-build hack/docker_build.sh        image name (default goslim-build)
#
# The files in dist/ belong to the calling user, not root.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
NOAUDIO="${NOAUDIO:-1}"
IMAGE="${IMAGE:-goslim-build}"

echo "==> docker build ${IMAGE} (version ${VERSION}, noaudio ${NOAUDIO})"
docker build -f docker/Dockerfile \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "NOAUDIO=${NOAUDIO}" \
  --build-arg "APPS=${APPS:-}" \
  --build-arg "TARGETS=${TARGETS:-}" \
  -t "$IMAGE" .

mkdir -p dist
echo "==> copying the artifacts to ./dist"
docker run --rm --user "$(id -u):$(id -g)" -v "$(pwd)/dist:/out" "$IMAGE"
echo "Done. Output is in dist/ (version ${VERSION})."
