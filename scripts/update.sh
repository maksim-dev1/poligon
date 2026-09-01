#!/usr/bin/env bash
# Pull latest poligon, rebuild, restart the launchd service.
# Run on the farm host from the repo root: scripts/update.sh
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"

echo "==> git pull"
git pull --ff-only

echo "==> build"
go build -o poligon ./cmd/poligon

LABEL=com.pancir.poligon
DOMAIN="gui/$(id -u)"
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
  echo "==> restart $LABEL"
  launchctl kickstart -k "$DOMAIN/$LABEL"
else
  echo "service not loaded — run scripts/install-service.sh once"
fi

echo "==> done"
