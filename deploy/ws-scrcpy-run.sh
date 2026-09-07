#!/usr/bin/env bash
# ProgramArguments for com.pancir.poligon-live. Runs every time launchd (re)starts
# the ws-scrcpy sidecar — crash-restart, `kickstart -k`, or boot. Makes the start
# idempotent: frees :8000, gives ws-scrcpy's adbkit a fresh adb server, and drops
# stale on-device scrcpy state so the goog-device tracker re-pushes cleanly.
set -uo pipefail
export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"

WS_DIR="${WS_SCRCPY_DIR:-$HOME/poligon-sidecar/ws-scrcpy}"

# 1. nothing else may hold the port
for pid in $(lsof -ti tcp:8000 -sTCP:LISTEN 2>/dev/null); do
  kill -9 "$pid" 2>/dev/null || true
done

# 2. fresh adb server (stale connections survive reboots and wedge the tracker)
adb kill-server 2>/dev/null || true
adb start-server 2>/dev/null || true
sleep 1

# 3. clear leftover scrcpy servers + pid files on every connected device
for serial in $(adb devices 2>/dev/null | awk 'NR>1 && $2=="device"{print $1}'); do
  adb -s "$serial" shell 'pkill -f com.genymobile.scrcpy.Server; rm -f /data/local/tmp/ws_scrcpy.pid' 2>/dev/null || true
done

cd "$WS_DIR" || { echo "ws-scrcpy dir not found: $WS_DIR" >&2; exit 1; }
exec /usr/local/bin/node dist/index.js
