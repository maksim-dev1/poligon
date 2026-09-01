#!/usr/bin/env bash
# One-time: install poligon as a per-user LaunchAgent on the farm host.
# Run from the repo root: scripts/install-service.sh
set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$(pwd)"
LABEL=com.pancir.poligon
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"

export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"

echo "==> initial build"
go build -o poligon ./cmd/poligon

echo "==> config"
[ -f config/devices.yaml ] || cp config/devices.example.yaml config/devices.yaml
mkdir -p config/profiles storage

echo "==> install LaunchAgent -> $PLIST"
mkdir -p "$(dirname "$PLIST")"
sed "s#/Users/dev-mac/poligon#$REPO#g" deploy/launchd/$LABEL.plist > "$PLIST"

launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootstrap "$DOMAIN" "$PLIST"
launchctl enable "$DOMAIN/$LABEL"

sleep 2
launchctl print "$DOMAIN/$LABEL" | grep -E "state =|pid =" || true
echo
echo "poligon should be on http://$(ipconfig getifaddr en0 2>/dev/null || echo localhost):8080"
echo "create an admin user:  ./poligon user add <name> --admin"
