#!/usr/bin/env bash
# Set the video settings the ws-scrcpy sidecar asks each Android device for, and
# rebuild it. Run on the farm host: scripts/tune-live-sidecar.sh
#
# Why this exists: ws-scrcpy's MSE player — the one the dashboard uses — ships
# defaults meant for looking at one phone full screen: 7 Mbit/s at 60 fps. The
# wall shows several phones at ~300px each, so that is several times the encode
# work on the phone, the USB traffic and the browser-side H.264 decoding that
# anyone actually sees. These numbers are a wall, not a cinema.
#
# Override per run, e.g.:
#   BITRATE=4000000 MAX_FPS=30 scripts/tune-live-sidecar.sh
set -euo pipefail
export PATH="/usr/local/bin:$HOME/opt/nodejs/bin:/opt/homebrew/bin:$PATH"

WS_DIR="${WS_SCRCPY_DIR:-$HOME/poligon-sidecar/ws-scrcpy}"
BITRATE="${BITRATE:-2000000}"      # bits/s per device
MAX_FPS="${MAX_FPS:-24}"
IFRAME_INTERVAL="${IFRAME_INTERVAL:-5}"   # seconds between keyframes
BOUNDS="${BOUNDS:-720}"            # longest edge the device encodes to

[ -d "$WS_DIR" ] || { echo "ws-scrcpy not found at $WS_DIR — run scripts/install-live-sidecar.sh first" >&2; exit 1; }

echo "==> patching player defaults: ${BITRATE}bps, ${MAX_FPS}fps, i-frame ${IFRAME_INTERVAL}s, bounds ${BOUNDS}"
python3 - "$WS_DIR" "$BITRATE" "$MAX_FPS" "$IFRAME_INTERVAL" "$BOUNDS" <<'PY'
import glob, os, re, sys

ws, bitrate, fps, iframe, bounds = sys.argv[1:6]
changed = []
for path in sorted(glob.glob(os.path.join(ws, "src/app/player/*.ts"))):
    src = open(path).read()
    if "preferredVideoSettings" not in src:
        continue
    out = src

    # rewrite the fields inside every `new VideoSettings({ ... })` literal that
    # belongs to a preferredVideoSettings declaration
    def patch(block):
        block = re.sub(r"bitrate:\s*\d+", "bitrate: " + bitrate, block)
        block = re.sub(r"maxFps:\s*\d+", "maxFps: " + fps, block)
        block = re.sub(r"iFrameInterval:\s*\d+", "iFrameInterval: " + iframe, block)
        block = re.sub(r"bounds:\s*new Size\(\d+,\s*\d+\)",
                       "bounds: new Size(%s, %s)" % (bounds, bounds), block)
        return block

    for m in re.finditer(r"preferredVideoSettings[^=]*=\s*new VideoSettings\(\{.*?\}\)", src, re.S):
        out = out.replace(m.group(0), patch(m.group(0)))

    if out != src:
        open(path, "w").write(out)
        changed.append(os.path.basename(path))

print("    patched:", ", ".join(changed) if changed else "nothing (already at these values)")
PY

echo "==> rebuilding ws-scrcpy (takes a minute)"
cd "$WS_DIR"
npm run dist >/dev/null

echo "==> restarting the sidecar (live Android screens blink)"
sudo launchctl kickstart -k system/com.pancir.poligon-live 2>/dev/null \
  || echo "   com.pancir.poligon-live not loaded — start it with scripts/install-all.sh"

echo "==> done"
