#!/usr/bin/env bash
# One-time: install poligon + ws-scrcpy sidecar + go-ios tunnel as managed
# services, plus log rotation. Run on the farm host from the repo root.
# Prerequisite: scripts/bootstrap-mac.sh (tools) and scripts/install-live-sidecar.sh
# (ws-scrcpy build) already run. For power-loss recovery see scripts/host-setup.sh.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$(pwd)"
UID_N="$(id -u)"
USER_N="$(id -un)"
export PATH="/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"

echo "==> build poligon"
go build -o poligon ./cmd/poligon
[ -f config/devices.yaml ] || cp config/devices.example.yaml config/devices.yaml
mkdir -p config/profiles storage
chmod +x deploy/ws-scrcpy-run.sh

render() { sed -e "s#/Users/dev-mac/poligon#$REPO#g" -e "s#<string>dev-mac</string>#<string>$USER_N</string>#g" "$1"; }

echo "==> poligon  (LaunchAgent, gui/$UID_N)"
AGENT="$HOME/Library/LaunchAgents/com.pancir.poligon.plist"
mkdir -p "$(dirname "$AGENT")"
render deploy/launchd/com.pancir.poligon.plist > "$AGENT"
launchctl bootout "gui/$UID_N/com.pancir.poligon" 2>/dev/null || true
launchctl bootstrap "gui/$UID_N" "$AGENT"
launchctl enable "gui/$UID_N/com.pancir.poligon"

echo "==> ws-scrcpy sidecar + go-ios tunnel  (system LaunchDaemons — sudo)"
for label in com.pancir.poligon-live com.pancir.go-ios-tunnel; do
  render "deploy/launchd/$label.plist" | sudo tee "/Library/LaunchDaemons/$label.plist" >/dev/null
  sudo chown root:wheel "/Library/LaunchDaemons/$label.plist"
  sudo launchctl bootout "system/$label" 2>/dev/null || true
  sudo launchctl bootstrap system "/Library/LaunchDaemons/$label.plist"
  sudo launchctl enable "system/$label"
done

echo "==> log rotation"
sudo cp deploy/newsyslog.d/pancir-poligon.conf /etc/newsyslog.d/pancir-poligon.conf

sleep 3
echo
echo "==> status"
scripts/farm-doctor.sh || true
echo
echo "Dashboard: http://$(ipconfig getifaddr en0 2>/dev/null || echo localhost):8080  (open it → Create account)"
echo "Power-loss auto-recovery: run scripts/host-setup.sh"
