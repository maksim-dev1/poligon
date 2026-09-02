#!/usr/bin/env bash
# Build + run WebDriverAgent on an iOS device and forward its ports to the host,
# so poligon can serve the device's live screen (ios_screen config).
#
# Usage: scripts/ios-wda.sh <UDID> [<WDA_LOCAL_PORT> <MJPEG_LOCAL_PORT>]
#
# Requires: Xcode with an Apple ID whose team can sign (set DEVELOPMENT_TEAM),
# libimobiledevice (iproxy). Device must have Developer Mode on and be unlocked.
set -euo pipefail

UDID="${1:?udid required}"
WDA_PORT="${2:-18100}"
MJPEG_PORT="${3:-19100}"
TEAM="${DEVELOPMENT_TEAM:?export DEVELOPMENT_TEAM=<10-char team id>}"
BUNDLE="${WDA_BUNDLE_ID:-com.poligon.WebDriverAgentRunner}"
WDA_SRC="${WDA_SRC:-$HOME/WebDriverAgent}"
DD="${WDA_DD:-/tmp/wda-dd}"

if [ ! -d "$WDA_SRC" ]; then
  git clone --depth 1 https://github.com/appium/WebDriverAgent.git "$WDA_SRC"
fi

echo "==> build WDA for $UDID (team $TEAM)"
xcodebuild build-for-testing \
  -project "$WDA_SRC/WebDriverAgent.xcodeproj" \
  -scheme WebDriverAgentRunner \
  -destination "id=$UDID" \
  -allowProvisioningUpdates -derivedDataPath "$DD" \
  DEVELOPMENT_TEAM="$TEAM" CODE_SIGN_STYLE=Automatic \
  PRODUCT_BUNDLE_IDENTIFIER="$BUNDLE" >/tmp/wda-build.log 2>&1

XCTESTRUN=$(ls "$DD"/Build/Products/WebDriverAgentRunner_*.xctestrun | head -1)

echo "==> run WDA (keep this process alive)"
nohup xcodebuild test-without-building -xctestrun "$XCTESTRUN" \
  -destination "id=$UDID" >/tmp/wda-run.log 2>&1 &
WDA_PID=$!

# wait for WDA's server line
for i in $(seq 1 60); do
  grep -q "ServerURLHere" /tmp/wda-run.log && break
  sleep 1
done
grep "ServerURLHere" /tmp/wda-run.log || { echo "WDA did not start (see /tmp/wda-run.log)"; exit 1; }

echo "==> forward ports  WDA:$WDA_PORT  MJPEG:$MJPEG_PORT"
pkill -f "iproxy .*$UDID" 2>/dev/null || true
nohup iproxy "$WDA_PORT:8100" -u "$UDID" >/tmp/iproxy-wda.log 2>&1 &
nohup iproxy "$MJPEG_PORT:9100" -u "$UDID" >/tmp/iproxy-mjpeg.log 2>&1 &
sleep 2

curl -sf "http://127.0.0.1:$WDA_PORT/status" >/dev/null && echo "WDA ready on :$WDA_PORT" || echo "WDA not responding"

cat <<EOF

Add to config/devices.yaml:

  ios_screen:
    <device-id>:
      wda: "127.0.0.1:$WDA_PORT"
      mjpeg: "127.0.0.1:$MJPEG_PORT"

WDA runner pid: $WDA_PID  (kill it to stop the screen)
EOF
