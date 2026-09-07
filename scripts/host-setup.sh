#!/usr/bin/env bash
# Make the farm host recover on its own after a power cut or reboot.
# Prints what it will change; run with --apply to actually do it. A reboot is
# required afterwards. Some steps (FileVault, auto-login) need a real admin at
# the machine — they cannot be done over SSH.
set -uo pipefail
APPLY=0
[ "${1:-}" = "--apply" ] && APPLY=1
run() { echo "  \$ $*"; [ "$APPLY" -eq 1 ] && sudo "$@"; }

echo "poligon host-setup  (apply=$APPLY)"
echo

echo "1. FileVault OFF  — an encrypted disk blocks *everything* (services, SSH)"
echo "   until someone types the password at the console after every boot."
if fdesetup status | grep -q "FileVault is On"; then
  echo "   currently: ON  →  needs turning off"
  run fdesetup disable
  echo "   (or: System Settings → Privacy & Security → FileVault → Turn Off)"
else
  echo "   currently: OFF  ✓"
fi
echo

echo "2. Auto-login for the farm user  — so the LaunchAgent (poligon) starts"
echo "   without anyone logging in. Available only once FileVault is off."
echo "   Do this in the GUI: System Settings → Users & Groups →"
echo "   'Automatically log in as' → $(id -un)."
echo "   Verify:  defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser"
echo

echo "3. Power / sleep"
run pmset -a autorestart 1   # power back on after a power failure
run pmset -a womp 1          # wake for network access
run pmset -a sleep 0         # farm host never sleeps
run pmset -a disksleep 0
echo

echo "4. Remote Login (SSH)  — for scripts/update.sh"
if systemsetup -getremotelogin 2>/dev/null | grep -q "On"; then
  echo "   currently: On  ✓"
else
  run systemsetup -setremotelogin on
fi
echo

[ "$APPLY" -eq 1 ] && echo "==> done — reboot to verify:  sudo reboot" \
                   || echo "==> dry run — re-run with --apply, then reboot"
