#!/usr/bin/env bash
# Adds a device UDID to the farm's ad-hoc provisioning profiles and refreshes
# the local copies poligon uses for re-signing.
#
# Usage: scripts/register-device.sh <UDID> <friendly-name> <app-id> [<app-id> ...]
#
# Requires: fastlane, an Apple Developer account with the farm's signing cert,
# APPLE_TEAM_ID and (optionally) FASTLANE_USER / FASTLANE_PASSWORD in the env.
set -euo pipefail

UDID="${1:?udid required}"
NAME="${2:?friendly name required}"
shift 2
APP_IDS=("$@")
[ "${#APP_IDS[@]}" -gt 0 ] || { echo "at least one app-id required"; exit 1; }

PROFILE_DIR="$(cd "$(dirname "$0")/../config/profiles" && pwd)"
mkdir -p "$PROFILE_DIR"

echo "==> Registering $NAME ($UDID)"
fastlane run register_devices devices:"{\"$NAME\":\"$UDID\"}"

for APP_ID in "${APP_IDS[@]}"; do
  echo "==> Regenerating ad-hoc profile for $APP_ID"
  fastlane run sigh \
    adhoc:true \
    force:true \
    app_identifier:"$APP_ID" \
    output_path:"$PROFILE_DIR" \
    filename:"$APP_ID.mobileprovision"
done

echo "==> Profiles in $PROFILE_DIR:"
ls -1 "$PROFILE_DIR"
