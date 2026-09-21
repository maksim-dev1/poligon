#!/usr/bin/env bash
# One-time: build the ws-scrcpy sidecar and install it + a pf rule that keeps
# its port host-local. Run on the farm host.
set -euo pipefail
export PATH="/usr/local/bin:$HOME/opt/nodejs/bin:$PATH"

DIR="$HOME/poligon-sidecar"
mkdir -p "$DIR" && cd "$DIR"

if [ ! -d ws-scrcpy ]; then
  git clone --depth 1 https://github.com/NetrisTV/ws-scrcpy.git
fi
cd ws-scrcpy
npm install --no-audit --no-fund

# a fresh clone carries ws-scrcpy's own defaults (7 Mbit/s at 60 fps per
# device), which are wrong for a wall of phones — patch them and build
"$(dirname "$0")/tune-live-sidecar.sh"

cat > "$DIR/ws-scrcpy.config.json" <<JSON
{ "server": [ { "secure": false, "port": 8000, "hostname": "127.0.0.1" } ],
  "runGoogTracker": true, "runApplTracker": false }
JSON

echo "ws-scrcpy built. Install the services with sudo:"
echo "  sudo cp $PWD/../../poligon/deploy/launchd/com.pancir.poligon-live.plist /Library/LaunchDaemons/"
echo "  sudo launchctl bootstrap system /Library/LaunchDaemons/com.pancir.poligon-live.plist"
echo "  # firewall: add anchor to /etc/pf.conf, then: sudo pfctl -e -f /etc/pf.conf"
