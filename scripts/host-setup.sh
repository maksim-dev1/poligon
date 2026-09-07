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

# Manual GUI steps — fdesetup/auto-login prompt for a FileVault user and can't
# be driven headlessly, so this script only checks and instructs.
FV_OK=1; AL_OK=1
echo "1. FileVault OFF  — an encrypted disk blocks *everything* (services, SSH)"
echo "   until someone types the password at the console after every boot."
if fdesetup status | grep -q "FileVault is On"; then
  FV_OK=0
  echo "   currently: ON  →  TURN OFF:  System Settings → Privacy & Security →"
  echo "   FileVault → Turn Off   (or:  sudo fdesetup disable)"
else
  echo "   currently: OFF  ✓"
fi
echo

echo "2. Auto-login for '$(id -un)'  — so the LaunchAgent (poligon) starts"
echo "   without anyone logging in. Available only once FileVault is off."
if [ "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null)" = "$(id -un)" ]; then
  echo "   currently: ON  ✓"
else
  AL_OK=0
  echo "   currently: OFF  →  SET:  System Settings → Users & Groups →"
  echo "   'Automatically log in as' → $(id -un)"
fi
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

echo
if [ "$FV_OK" -eq 0 ] || [ "$AL_OK" -eq 0 ]; then
  echo "==> still needed (GUI): $([ $FV_OK -eq 0 ] && echo 'FileVault off') $([ $AL_OK -eq 0 ] && echo 'auto-login')"
fi
[ "$APPLY" -eq 1 ] && echo "==> pmset/ssh applied — reboot to verify:  sudo reboot && scripts/farm-doctor.sh" \
                   || echo "==> dry run — re-run with --apply for pmset/ssh"
