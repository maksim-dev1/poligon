#!/usr/bin/env bash
# ProgramArguments for com.pancir.poligon-live. Runs every time launchd (re)starts
# the ws-scrcpy sidecar — crash-restart, `kickstart -k`, or boot. Makes the start
# idempotent: frees :8000, waits for the farm's adb server (com.pancir.adb), and
# drops stale on-device scrcpy state so the goog-device tracker re-pushes cleanly.
set -uo pipefail
export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"  # lsof lives in /usr/sbin

WS_DIR="${WS_SCRCPY_DIR:-$HOME/poligon-sidecar/ws-scrcpy}"

# 1. nothing else may hold the port
for pid in $(lsof -ti tcp:8000 -sTCP:LISTEN 2>/dev/null); do
  kill -9 "$pid" 2>/dev/null || true
done

# bounded adb call: a wedged server must not hang this start forever
t() { perl -e 'alarm shift; exec @ARGV' "$@"; }

# 2. wait for the adb server. It belongs to com.pancir.adb (launchd) — never
#    kill or start it here: a server started from this script daemonizes, is
#    owned by nobody, and was how the farm ended up with two wedged servers.
#    `adb` would auto-start one if we asked before it listens, so wait on the
#    port first. Hosts without com.pancir.adb keep the old behaviour.
for _ in $(seq 1 60); do
  lsof -ti tcp:5037 -sTCP:LISTEN >/dev/null 2>&1 && break
  sleep 1
done
if ! lsof -ti tcp:5037 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "adb server not listening after 60s (is com.pancir.adb installed?) — starting one" >&2
  t 15 adb start-server 2>/dev/null || true
fi

# 3. clear leftover scrcpy servers + pid files on every connected device
for serial in $(t 10 adb devices 2>/dev/null | awk 'NR>1 && $2=="device"{print $1}'); do
  t 10 adb -s "$serial" shell 'pkill -f com.genymobile.scrcpy.Server; rm -f /data/local/tmp/ws_scrcpy.pid' 2>/dev/null || true
done

cd "$WS_DIR" || { echo "ws-scrcpy dir not found: $WS_DIR" >&2; exit 1; }
exec /usr/local/bin/node dist/index.js
