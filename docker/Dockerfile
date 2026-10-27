# Builds GoSlimPlayer, GoSlim and GoUPnPAudioClient inside a Go 1.26 container.
# Build it from the repository root; hack/docker_build.sh does this and copies the
# results out:
#
#   docker build -f docker/Dockerfile --build-arg VERSION=1.2.3 --build-arg NOAUDIO=1 -t goslim-build .
#   docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/dist:/out" goslim-build
#
# Build arguments:
#   VERSION  version injected into the programs (default "dev")
#   NOAUDIO  1 (default) = Linux binaries without local audio (-tags noaudio): UPnP and proxy
#            modes only, no cgo. 0 = link ALSA local audio into the Linux binaries; the
#            image then installs libasound2-dev and builds only the Linux targets that can
#            be compiled natively with cgo (amd64 and the build host's arch), plus Windows/macOS.
#   APPS     apps to build (default: all three)
#   TARGETS  os/arch list (default: build.sh's default; with NOAUDIO=0 linux/arm64 is left out)
FROM golang:1.26-bookworm

ARG VERSION=dev
ARG NOAUDIO=1
ARG APPS=
ARG TARGETS=

WORKDIR /src
COPY . .

# gzip is for build.sh's .gz outputs; the ALSA headers only matter with NOAUDIO=0.
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends gzip git ca-certificates; \
    if [ "$NOAUDIO" = "0" ]; then apt-get install -y --no-install-recommends gcc libc6-dev libasound2-dev; fi; \
    rm -rf /var/lib/apt/lists/*

# build.sh runs go get / go mod tidy for the dependencies, then builds every target into dist/.
RUN set -eu; \
    export VERSION="$VERSION"; \
    [ -z "$APPS" ] || export APPS="$APPS"; \
    if [ "$NOAUDIO" = "0" ]; then \
      export LINUX_AUDIO=1; \
      export TARGETS="${TARGETS:-windows/amd64 windows/386 linux/amd64 darwin/arm64 darwin/amd64}"; \
    else \
      [ -z "$TARGETS" ] || export TARGETS="$TARGETS"; \
    fi; \
    bash hack/build.sh

# Mount a host directory on /out to get the artifacts.
CMD ["sh", "-c", "mkdir -p /out && cp -r /src/dist/. /out/ && ls -l /out"]
