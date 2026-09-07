#!/usr/bin/env bash
# Health report for the phone farm. `--fix` also cleans up and restarts services.
# Run on the farm host: scripts/farm-doctor.sh [--fix]
set -uo pipefail
export PATH="/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"

FIX=0
[ "${1:-}" = "--fix" ] && FIX=1
UID_N="$(id -u)"
cd "$(dirname "$0")/.."

hr() { printf '%s\n' "----------------------------------------"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; }

echo "poligon farm doctor  ($(date '+%F %T'))"; hr

# --- services ---
echo "services"
for svc in "gui/$UID_N/com.pancir.poligon" system/com.pancir.poligon-live system/com.pancir.go-ios-tunnel; do
  case "$svc" in system/*) L="sudo launchctl";; *) L="launchctl";; esac
  if $L print "$svc" 2>/dev/null | grep -q "state = running"; then ok "$svc"; else bad "$svc not running"; fi
done

# --- endpoints ---
echo "endpoints"
curl -sf --max-time 3 -o /dev/null http://127.0.0.1:8080/       && ok "poligon :8080"      || bad "poligon :8080"
curl -sf --max-time 3 -o /dev/null http://127.0.0.1:8000/       && ok "ws-scrcpy :8000"    || bad "ws-scrcpy :8000"
ios tunnel ls >/dev/null 2>&1                                    && ok "go-ios tunnel"      || bad "go-ios tunnel"

H=$(curl -sf --max-time 4 http://127.0.0.1:8080/healthz 2>/dev/null || true)
[ -n "$H" ] && { echo "healthz"; echo "$H" | python3 -m json.tool 2>/dev/null | sed 's/^/  /'; }

# --- adb ---
echo "adb devices"
adb devices | awk 'NR>1 && NF {printf "  %s  %s\n", $1, $2}'
UNAUTH=$(adb devices | awk 'NR>1 && $2!="device" && $2!="" {print $1}')
[ -n "$UNAUTH" ] && warn "not usable: $UNAUTH (unlock / replug / approve trust)"

# --- orphans / stale ports ---
echo "processes"
RW=$(pgrep -fc "ios runwda" 2>/dev/null || echo 0)
FW=$(pgrep -fc "ios forward" 2>/dev/null || echo 0)
echo "  ios runwda: $RW   ios forward: $FW"
for p in 18100 18101 19100 19101; do
  H2=$(lsof -ti tcp:$p -sTCP:LISTEN 2>/dev/null || true)
  [ -n "$H2" ] && echo "  :$p held by pid $H2"
done

# --- logs ---
echo "logs"
for f in /Users/dev-mac/poligon/poligon.err.log /Users/dev-mac/poligon-sidecar/ws-scrcpy.err.log /var/log/com.pancir.go-ios-tunnel.err.log; do
  [ -f "$f" ] && printf "  %6s  %s\n" "$(du -h "$f" | cut -f1)" "$f"
done

hr
if [ "$FIX" -eq 0 ]; then
  echo "run with --fix to clean up and restart"
  exit 0
fi

echo "==> FIX"
pkill -f "ios runwda"  2>/dev/null || true
pkill -f "ios forward" 2>/dev/null || true
adb kill-server 2>/dev/null || true; adb start-server 2>/dev/null || true
sudo launchctl kickstart -k system/com.pancir.go-ios-tunnel 2>/dev/null || true
sudo launchctl kickstart -k system/com.pancir.poligon-live  2>/dev/null || true
launchctl kickstart -k "gui/$UID_N/com.pancir.poligon"      2>/dev/null || true
for f in /Users/dev-mac/poligon/poligon.err.log /Users/dev-mac/poligon-sidecar/ws-scrcpy.err.log; do
  [ -f "$f" ] && [ "$(stat -f%z "$f")" -gt 52428800 ] && : > "$f" && echo "  truncated $f"
done
sleep 5
echo "==> re-check:"; exec "$0"
