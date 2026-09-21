#!/usr/bin/env bash
# Deploy the latest poligon: pull, build atomically, restart every service, wait
# for /healthz. Run on the farm host from the repo root: scripts/update.sh
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"

UID_N="$(id -u)"

# Restarting a LaunchDaemon needs root. Over ssh without a tty, a cached sudo
# ticket does not always carry into this script, and the old code printed
# "not loaded" for that case too — so a deploy looked like it had restarted the
# sidecar when it had not. Tell the truth, and say exactly what to run.
restart_daemon() {
  local label="$1"
  if ! sudo -n launchctl print "system/$label" >/dev/null 2>&1; then
    if ! launchctl print "system/$label" >/dev/null 2>&1; then
      echo "   $label is not loaded — install it with scripts/install-all.sh"
      return 0
    fi
  fi
  if sudo -n launchctl kickstart -k "system/$label" >/dev/null 2>&1; then
    echo "   restarted $label"
    return 0
  fi
  echo "   could NOT restart $label (needs sudo). Run this on the host:"
  echo "     sudo launchctl kickstart -k system/$label"
  return 0
}

echo "==> git pull"
git pull --ff-only

echo "==> build (atomic)"
go build -o poligon.new ./cmd/poligon
mv poligon.new poligon          # only swapped if the build succeeded

echo "==> restart services"
launchctl kickstart -k "gui/$UID_N/com.pancir.poligon" 2>/dev/null \
  || echo "   poligon not loaded — run scripts/install-all.sh"
restart_daemon com.pancir.poligon-live
restart_daemon com.pancir.go-ios-tunnel

echo "==> waiting for /healthz"
for i in $(seq 1 20); do
  if H=$(curl -sf --max-time 3 http://127.0.0.1:8080/healthz); then
    echo "$H" | python3 -m json.tool 2>/dev/null || echo "$H"
    ok=$(printf '%s' "$H" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("ok"))' 2>/dev/null || echo "?")
    [ "$ok" = "True" ] && { echo "==> all green"; exit 0; }
    echo "==> poligon up, some deps still settling (attempt $i)"
    sleep 3
  else
    sleep 2
  fi
done
echo "==> poligon did not report healthy — check: scripts/farm-doctor.sh"
exit 1
