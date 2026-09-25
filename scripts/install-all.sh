#!/usr/bin/env bash
# One-time: install poligon + adb server + ws-scrcpy sidecar + go-ios tunnel as
# managed services, a sudoers rule for restarting the daemons, and log rotation. Run on the farm host from the repo root.
# Prerequisite: scripts/bootstrap-mac.sh (tools) and scripts/install-live-sidecar.sh
# (ws-scrcpy build) already run. For power-loss recovery see scripts/host-setup.sh.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$(pwd)"
UID_N="$(id -u)"
USER_N="$(id -un)"
export PATH="/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

echo "==> build poligon"
go build -o poligon ./cmd/poligon
[ -f config/devices.yaml ] || cp config/devices.example.yaml config/devices.yaml
mkdir -p config/profiles storage
chmod +x deploy/ws-scrcpy-run.sh deploy/adb-server-run.sh

render() { sed -e "s#/Users/dev-mac/poligon#$REPO#g" -e "s#<string>dev-mac</string>#<string>$USER_N</string>#g" "$1"; }

# (re)load a service idempotently: replace the plist, then bootstrap if it isn't
# loaded, else kickstart it.
reload_agent() {  # domain plistpath label
  local dom=$1 plist=$2 label=$3
  if launchctl print "$dom/$label" >/dev/null 2>&1; then
    launchctl kickstart -k "$dom/$label"
  else
    launchctl bootstrap "$dom" "$plist" && launchctl enable "$dom/$label"
  fi
}
reload_daemon() { # plistpath label
  local plist=$1 label=$2
  if sudo launchctl print "system/$label" >/dev/null 2>&1; then
    sudo launchctl kickstart -k "system/$label"
  else
    sudo launchctl bootstrap system "$plist" && sudo launchctl enable "system/$label"
  fi
}

echo "==> adb server  (LaunchAgent, gui/$UID_N) — the farm's only adb server"
ADB_AGENT="$HOME/Library/LaunchAgents/com.pancir.adb.plist"
mkdir -p "$(dirname "$ADB_AGENT")"
render deploy/launchd/com.pancir.adb.plist > "$ADB_AGENT"
reload_agent "gui/$UID_N" "$ADB_AGENT" com.pancir.adb

echo "==> sudoers: let $USER_N restart the farm daemons without a password"
# poligon's watchdog restarts ws-scrcpy after the adb server changes, and
# update.sh restarts both daemons — over ssh/launchd there is no tty for a
# password. The rule allows exactly these two commands, nothing else.
SUDOERS=/etc/sudoers.d/pancir-poligon
TMP=$(mktemp)
cat > "$TMP" <<RULES
# installed by poligon scripts/install-all.sh
$USER_N ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/com.pancir.poligon-live, /bin/launchctl kickstart -k system/com.pancir.go-ios-tunnel, /bin/launchctl print system/com.pancir.poligon-live, /bin/launchctl print system/com.pancir.go-ios-tunnel
RULES
if sudo visudo -cf "$TMP" >/dev/null; then
  sudo install -m 0440 -o root -g wheel "$TMP" "$SUDOERS"
else
  echo "   sudoers rule failed validation — not installed" >&2
fi
rm -f "$TMP"

echo "==> poligon  (LaunchAgent, gui/$UID_N)"
AGENT="$HOME/Library/LaunchAgents/com.pancir.poligon.plist"
mkdir -p "$(dirname "$AGENT")"
render deploy/launchd/com.pancir.poligon.plist > "$AGENT"
reload_agent "gui/$UID_N" "$AGENT" com.pancir.poligon

echo "==> ws-scrcpy sidecar + go-ios tunnel  (system LaunchDaemons — sudo)"
sudo mkdir -p /Users/Shared/go-ios   # go-ios tunnel needs a writable cwd/HOME
for label in com.pancir.poligon-live com.pancir.go-ios-tunnel; do
  render "deploy/launchd/$label.plist" | sudo tee "/Library/LaunchDaemons/$label.plist" >/dev/null
  sudo chmod 644 "/Library/LaunchDaemons/$label.plist"
  reload_daemon "/Library/LaunchDaemons/$label.plist" "$label"
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
