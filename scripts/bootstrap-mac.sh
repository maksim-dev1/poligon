#!/usr/bin/env bash
# Provisions the farm host (Mac mini) with everything poligon needs.
# Idempotent: safe to re-run. Assumes Homebrew is installed and on PATH.
set -euo pipefail

echo "==> Homebrew packages"
brew update
brew install \
  android-platform-tools \
  libimobiledevice \
  ios-deploy \
  go \
  node \
  bundletool

# Maestro (not in Homebrew core)
if ! command -v maestro >/dev/null; then
  echo "==> Maestro"
  curl -fsSL "https://get.maestro.mobile.dev" | bash
fi

# fastlane for iOS re-signing
if ! command -v fastlane >/dev/null; then
  echo "==> fastlane"
  brew install fastlane
fi

# Appium + drivers (phase 3; comment out if not needed yet)
if ! command -v appium >/dev/null; then
  echo "==> Appium"
  npm install -g appium
  appium driver install uiautomator2 || true
  appium driver install xcuitest     || true
fi

echo "==> Versions"
adb --version | head -1
idevice_id -h 2>&1 | head -1 || true
ios-deploy --version || true
go version
node --version
maestro --version || true
fastlane --version | head -1 || true

echo
echo "Done. Next:"
echo "  1. Wire devices, fill config/devices.yaml (adb devices / idevice_id -l)"
echo "  2. For iOS: put farm ad-hoc *.mobileprovision in config/profiles/,"
echo "     export POLIGON_SIGNING_IDENTITY and POLIGON_BUNDLETOOL"
echo "  3. ./poligon serve"
