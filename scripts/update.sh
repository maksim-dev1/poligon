#!/usr/bin/env bash
# Deploy the latest poligon: pull, build atomically, restart every service, wait
# for /healthz. Run on the farm host from the repo root: scripts/update.sh
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"

UID_N="$(id -u)"

echo "==> git pull"
git pull --ff-only

echo "==> build (atomic)"
go build -o poligon.new ./cmd/poligon
mv poligon.new poligon          # only swapped if the build succeeded

echo "==> restart services"
launchctl kickstart -k "gui/$UID_N/com.pancir.poligon" 2>/dev/null \
  || echo "   poligon not loaded — run scripts/install-all.sh"
sudo launchctl kickstart -k system/com.pancir.poligon-live 2>/dev/null \
  || echo "   com.pancir.poligon-live not loaded"
sudo launchctl kickstart -k system/com.pancir.go-ios-tunnel 2>/dev/null \
  || echo "   com.pancir.go-ios-tunnel not loaded"

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
