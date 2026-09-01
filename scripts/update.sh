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
if [ -f "/Library/LaunchDaemons/$LABEL.plist" ]; then
  echo "==> restart $LABEL"
  sudo launchctl kickstart -k "system/$LABEL"
elif [ -f "$HOME/Library/LaunchAgents/$LABEL.plist" ]; then
  echo "==> restart $LABEL (agent)"
  launchctl kickstart -k "gui/$(id -u)/$LABEL"
else
  echo "service not loaded — run scripts/install-service.sh once"
fi

echo "==> done"
