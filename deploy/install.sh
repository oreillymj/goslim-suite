#!/usr/bin/env bash
# Install GoSlimPlayer as a systemd service on Linux. Run as root (sudo deploy/install.sh).
#
#   sudo ./install.sh [path/to/goslimplayer[.gz]] [path/to/config.json]
#
# Defaults: ./goslimplayer, ./goslimplayer.gz or ../dist/goslimplayer-*-linux-*.gz (the newest);
# ./config.json. An existing /etc/goslimplayer/config.json is never overwritten. Re-running the
# script upgrades the binary and the unit and restarts the service.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "This script must be run as root, or with sudo: sudo $0 $*" >&2
  exit 1
fi
command -v systemctl >/dev/null 2>&1 || { echo "systemd (systemctl) not found; see the header of goslimplayer.service for a manual install." >&2; exit 1; }

here="$(cd "$(dirname "$0")" && pwd)"
USER_NAME=goslim
BIN_DIR=/opt/goslimplayer
CONF_DIR=/etc/goslimplayer
UNIT=/etc/systemd/system/goslimplayer.service

bin="${1:-}"
if [ -z "$bin" ]; then
  for c in "$PWD/goslimplayer" "$PWD/goslimplayer.gz" "$here/goslimplayer" "$here/goslimplayer.gz"; do
    [ -f "$c" ] && { bin="$c"; break; }
  done
fi
if [ -z "$bin" ]; then
  bin="$(ls -t "$here"/../dist/goslimplayer-*-linux-*.gz 2>/dev/null | head -n1 || true)"
fi
[ -n "$bin" ] && [ -f "$bin" ] || { echo "Cannot find the goslimplayer binary; pass its path as the first argument." >&2; exit 1; }

conf="${2:-}"
if [ -z "$conf" ]; then
  for c in "$PWD/config.json" "$here/config.json"; do
    [ -f "$c" ] && { conf="$c"; break; }
  done
fi
if [ ! -f "$CONF_DIR/config.json" ] && { [ -z "$conf" ] || [ ! -f "$conf" ]; }; then
  echo "No config.json given and none installed in $CONF_DIR; pass one as the second argument." >&2
  exit 1
fi
[ -f "$here/goslimplayer.service" ] || { echo "goslimplayer.service not found next to this script ($here)." >&2; exit 1; }

echo "==> user $USER_NAME"
if ! id "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$USER_NAME"
fi

echo "==> $BIN_DIR/goslimplayer (from $bin)"
install -d "$BIN_DIR" "$CONF_DIR"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
case "$bin" in
  *.gz) gunzip -c "$bin" > "$tmp" ;;
  *)    cat "$bin" > "$tmp" ;;
esac
install -m 755 -o "$USER_NAME" -g root "$tmp" "$BIN_DIR/goslimplayer"

if [ -f "$CONF_DIR/config.json" ]; then
  echo "==> keeping the existing $CONF_DIR/config.json"
else
  echo "==> $CONF_DIR/config.json (from $conf)"
  install -m 640 -o "$USER_NAME" -g root "$conf" "$CONF_DIR/config.json"
fi
# The program writes renderer IP changes back to config.json, so the service user owns it.
chown "$USER_NAME":root "$CONF_DIR/config.json"

echo "==> $UNIT"
install -m 644 "$here/goslimplayer.service" "$UNIT"
systemctl daemon-reload
systemctl enable goslimplayer
systemctl restart goslimplayer

echo
echo "Installed. Follow the log with:  journalctl -u goslimplayer -f"
echo "Run it by hand as the service user with:"
echo "  sudo runuser -u $USER_NAME -- $BIN_DIR/goslimplayer -u $CONF_DIR/config.json"
