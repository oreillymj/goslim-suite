#!/usr/bin/env bash
# Build GoSlimPlayer, GoSlim and GoUPnPAudioClient into dist/.
#
#   hack/build.sh                      all apps, all targets
#   APPS=goslimplayer hack/build.sh    one app
#   TARGETS="linux/arm64" hack/build.sh
#   VERSION=1.2.3 hack/build.sh        default: git describe, or "dev"
#   SKIP_DEPS=1 hack/build.sh          skip go get / go mod tidy (offline builds)
#   LINUX_AUDIO=1 hack/build.sh        link local audio (ALSA) into the Linux builds;
#                                      needs libasound2-dev and a native Linux host
#
# Local audio (the oto library) builds without cgo on Windows and macOS. On
# Linux it needs cgo and the ALSA headers, so the Linux builds are made with
# -tags noaudio (UPnP/proxy modes only) unless LINUX_AUDIO=1.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=${VERSION}"
APPS="${APPS:-goslimplayer goslim goupnpaudioclient}"
TARGETS="${TARGETS:-windows/amd64 windows/386 linux/amd64 linux/arm64 darwin/arm64 darwin/amd64}"
LINUX_AUDIO="${LINUX_AUDIO:-0}"

# External dependencies: fetched at the versions GoSlim was tested with, then
# go mod tidy writes go.mod/go.sum. Set SKIP_DEPS=1 to build offline from what
# is already downloaded.
DEPS="${DEPS:-github.com/ebitengine/oto/v3@v3.5.1
github.com/hajimehoshi/go-mp3@v0.3.4
github.com/mewkiz/flac@v1.0.14
github.com/tphakala/go-aac@v0.7.1
github.com/jfreymuth/oggvorbis@v1.0.5
golang.org/x/net@v0.50.0}"
if [ "${SKIP_DEPS:-0}" != "1" ]; then
  for dep in $DEPS; do
    echo "==> go get $dep"
    go get "$dep"
  done
  echo "==> go mod tidy"
  go mod tidy
fi

mkdir -p dist
for APP in $APPS; do
  PKG="./cmd/${APP}"
  for target in $TARGETS; do
    GOOS="${target%/*}"
    GOARCH="${target#*/}"
    EXT=""; TAGS=""; CGO=0
    [ "$GOOS" = "windows" ] && EXT=".exe"
    if [ "$GOOS" = "linux" ]; then
      if [ "$LINUX_AUDIO" = "1" ]; then CGO=1; else TAGS="noaudio"; fi
    fi
    OUT="dist/${APP}-${VERSION}-${GOOS}-${GOARCH}${EXT}"
    echo "==> ${OUT}${TAGS:+  (tags: ${TAGS})}"
    GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO" \
      go build -trimpath -tags "$TAGS" -ldflags "$LDFLAGS" -o "$OUT" "$PKG"
    if [ "$GOOS" != "windows" ]; then
      gzip -9 -c "$OUT" > "${OUT}.gz"
      rm "$OUT"
    fi
  done
done
echo "Done. Output is in dist/ (version ${VERSION})."
