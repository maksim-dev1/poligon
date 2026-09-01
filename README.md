# poligon

Self-hosted phone farm for testing built Flutter apps (`.apk` / `.aab` / `.ipa`)
on real wired devices. poligon does **not** build apps — it takes finished
artifacts, installs them on reserved devices, and lets you test.

Host: Mac mini (`ssh admin@172.24.17.30`). Single Go binary + SQLite, no Redis/Postgres.

## Status — Phase 1 (device catalog + reservations + manual install)

Done:
- device inventory from `config/devices.yaml`, health poll (adb / libimobiledevice), flap → `degraded`
- hardware specs per device (model, SoC, RAM, screen, battery, OS)
- users + bearer tokens (`poligon user add`)
- reservations: one holder per device, heartbeat lease, idle + hard-cap auto-release
- manual install via dashboard / API: apk direct, aab via bundletool, ipa re-signed with farm profiles then `ios-deploy`
- minimal dashboard at `/`

Next:
- Phase 2 — live screen (ws-scrcpy for Android, WDA/screenshot for iOS), multi-device select
- Phase 3 — automated jobs (`install_only` / `maestro` / `appium`), CI webhooks

## Run

```sh
cp config/devices.example.yaml config/devices.yaml   # edit: real serials / udids
go build -o poligon ./cmd/poligon
./poligon user add me --admin                        # prints a token
POLIGON_DEV_USER=me ./poligon serve                  # dev: skip auth
# open http://localhost:8080
```

### Environment

| var | meaning |
|---|---|
| `POLIGON_CONFIG` | config path (default `config/devices.yaml`) |
| `POLIGON_DEV_USER` | bypass auth, treat all requests as this user (dev only) |
| `POLIGON_BUNDLETOOL` | path to `bundletool.jar` for `.aab` |
| `POLIGON_SIGNING_IDENTITY` | codesign identity, e.g. `Apple Distribution: Company (TEAMID)` |
| `POLIGON_PROFILE_DIR` | farm `.mobileprovision` dir (default `config/profiles`) |

## iOS re-signing

An `.ipa` installs only if signed with a profile covering the target device.
poligon re-signs incoming builds with the farm's **ad-hoc** profiles (one
`<bundle-id>.mobileprovision` per app + extension in `config/profiles/`), using
the company signing identity. Same Team ID ⇒ entitlements (push, App Groups,
deeplinks) survive. Register new device UDIDs with `scripts/register-device.sh`.

## Host setup

`scripts/bootstrap-mac.sh` — installs adb, libimobiledevice, ios-deploy, go,
node, bundletool, maestro, fastlane, appium.

`deploy/launchd/com.pancir.poligon.plist` — run poligon as a launchd daemon.

## Layout

```
cmd/poligon        entrypoint + CLI
internal/config    devices.yaml loader
internal/store     sqlite (schema.sql embedded)
internal/model     domain types
internal/adb       adb wrapper (list, specs, install)
internal/ios       libimobiledevice + ios-deploy wrapper
internal/devices   poll loop, flap detection, specs refresh
internal/reserve   booking, leases, auto-release
internal/auth      users + bearer tokens
internal/install   apk / aab / ipa(re-sign) install pipeline
internal/api       JSON API + dashboard
internal/webui     embedded dashboard assets
```
