#!/usr/bin/env bash
# ProgramArguments for com.pancir.adb — the farm's one and only adb server.
#
# adb normally starts its server on demand from whichever client runs first and
# daemonizes it, so it belongs to no service: no restart or shutdown ever stops
# it. A wedged server then outlives every restart, `adb kill-server` hangs on
# it, the next client spawns a second server beside it, and the old one keeps
# the phones' USB interfaces — adb lists nothing while the phones are plugged
# in. Here launchd owns the server instead: `server nodaemon` stays in the
# foreground, so launchd's stop/kickstart really stops it, and every start first
# removes any other adb server so there is never more than one.
set -uo pipefail
export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"

ADB="${ADB:-$(command -v adb)}"
PORT="${ADB_SERVER_PORT:-5037}"

# every adb server/client left on the host, whoever started it — never ask a
# possibly wedged server to quit (`adb kill-server` hangs on one), just kill it
pkill -9 -x adb 2>/dev/null || true
for _ in 1 2 3 4 5 6 7 8 9 10; do
  pgrep -x adb >/dev/null || break
  sleep 0.5
done
# the port must be free, or the new server exits and launchd loops
for pid in $(lsof -ti "tcp:$PORT" -sTCP:LISTEN 2>/dev/null); do
  kill -9 "$pid" 2>/dev/null || true
done

echo "$(date '+%F %T') starting adb server on tcp:$PORT"
exec "$ADB" -L "tcp:$PORT" server nodaemon
