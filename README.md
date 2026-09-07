# poligon

Self-hosted phone farm for testing built Flutter apps (`.apk` / `.aab` / `.ipa`)
on real wired devices. poligon does **not** build apps — it takes finished
artifacts, installs them on reserved devices, and lets you test.

Host: Mac mini (`ssh dev-mac@172.24.18.20`). Single Go binary + SQLite, no Redis/Postgres.

## Status

Done:
- device inventory from `config/devices.yaml`, health poll (adb / libimobiledevice), flap → `degraded`
- hardware specs per device (model, SoC, RAM, screen, battery, OS)
- auth: open self-service signup (email + password), server-side sessions, no admin role; personal API tokens for scripts / CI
- reservations: one holder per device, heartbeat lease, idle + hard-cap auto-release; multi-device batches
- manual install via dashboard / API: apk direct, aab via bundletool, ipa re-signed with farm profiles then `ios-deploy`; mixed Android+iOS batches take one artifact per platform
- live screens: ws-scrcpy for Android, WebDriverAgent for iOS, one grid for a batch
- per-device diagnostics: screenshot + logcat from the grid (`internal/capture`)
- **test runs** (`internal/runner`): `install_smoke` (install → launch → assert alive + no crash) and `maestro` (run a `.yaml` flow, collect report + recording). Per-device artifacts under `<storage_dir>/runs/<id>/<device>/`, results at `/runs.html`

Next:
- more run types — Flutter `integration_test`, generic `appium` / command
- run history filters, status badge

### Test-run API

```sh
# mint a token on the host
poligon token create you@company.com ci

# smoke-test a build on 2 free Android devices
curl -sX POST https://farm/api/runs \
  -H "Authorization: Bearer plgn_…" \
  -F type=install_smoke -F platform=android -F count=2 \
  -F artifact=@app-release.apk

# maestro flow, build pulled from CI artifact storage, callback on finish
curl -sX POST https://farm/api/runs \
  -H "Authorization: Bearer plgn_…" \
  --data-urlencode type=maestro --data-urlencode platform=android --data-urlencode count=1 \
  --data-urlencode artifact_url=https://ci/…/app.apk \
  --data-urlencode flow_url=https://ci/…/flow.yaml \
  --data-urlencode callback_url=https://ci/…/hook

# generic: run any command, ANDROID_SERIAL / DEVICE_UDID / POLIGON_RUN_DIR in env
curl -sX POST https://farm/api/runs -H "Authorization: Bearer plgn_…" \
  --data-urlencode type=command --data-urlencode device=pixel6-01 \
  --data-urlencode 'command=appium ... || exit 1'

# poll: GET /api/runs/{id} → {status: queued|running|passed|failed|error|canceled, devices:[…]}
```

Run types: `install_smoke`, `maestro`, `command`. Device selection is
`device=<id>` (repeatable) **or** `platform=`/`count=`/`tag=`. Per-device cap
`timeout_seconds` (default 1200). Artifacts:
`GET /api/runs/{id}/artifacts/{device}/{path}`. Status SVG for a CI dashboard:
`GET /runs/{id}/badge.svg` (no auth). Use `--data-urlencode` for urlencoded
bodies (a raw `;` or space is rejected).

## Run

```sh
cp config/devices.example.yaml config/devices.yaml   # edit: real serials / udids
go build -o poligon ./cmd/poligon
POLIGON_DEV_USER=me@company.com ./poligon serve --dev # dev: bypass auth
# open http://localhost:8080 → "Create account"
```

## Authentication

**Open registration.** Anyone who can reach the dashboard clicks *Create account*,
picks an **email + password**, and is in. There is no admin role and no invite
step — the network (LAN / VPN) is the perimeter. Passwords are bcrypt hashed.

- Forgot your password? Someone with shell access to the host runs
  `poligon user reset-password <email>` and hands you the one-time link it prints.
- `poligon user list | disable <email> | enable <email> | reset-password <email> | add <email>`
  — host-side moderation. `disable` and `reset-password` kill the user's active
  sessions immediately. A password change from the dashboard drops the user's
  other sessions too.
- Sessions live server-side in SQLite: `HttpOnly` cookie, 14-day cap with a
  24h sliding idle window, revoked on logout. CSRF is enforced (double-submit)
  on cookie-authenticated writes.
- Login and signup are rate-limited (5 failures per email/IP → 15-min lock).
- Legacy `Authorization: Bearer <token>` still resolves for pre-existing
  scripted callers; personal API tokens are a separate follow-up.

Put poligon behind TLS for anything past the trusted LAN — either set `tls:` in
the config (direct HTTPS) or front it with `tailscale serve` / Caddy. Secure
cookies switch on automatically when the request arrives over HTTPS.

### Environment

| var | meaning |
|---|---|
| `POLIGON_CONFIG` | config path (default `config/devices.yaml`) |
| `POLIGON_DEV_USER` | bypass auth as this user; honored **only** on a loopback `listen` or with `serve --dev` |
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

`scripts/install-live-sidecar.sh` — builds the ws-scrcpy sidecar.

`scripts/install-all.sh` — installs the three services (below) + log rotation.

`scripts/host-setup.sh` — the power-loss recovery settings (needs a reboot).

## Operations

Three services keep the farm running. All are `KeepAlive` and start at boot;
each (re)start is self-cleaning.

| service | what | logs |
|---|---|---|
| `com.pancir.poligon` (LaunchAgent, gui) | the Go binary — API, dashboard, device poll, iOS WebDriverAgent | `~/poligon/poligon.{out,err}.log` |
| `com.pancir.poligon-live` (LaunchDaemon) | ws-scrcpy sidecar for Android screens; runs `deploy/ws-scrcpy-run.sh` which frees `:8000` + resets adb + clears stale on-device state on every start | `~/poligon-sidecar/ws-scrcpy.{out,err}.log` |
| `com.pancir.go-ios-tunnel` (LaunchDaemon, root) | go-ios tunnel — required for iOS 17+ | `/var/log/com.pancir.go-ios-tunnel.{out,err}.log` |

- **Deploy:** `scripts/update.sh` — pull, atomic build (a broken build never
  replaces the running binary), restart all three, wait for `/healthz`.
- **Diagnose:** `scripts/farm-doctor.sh` (report) / `--fix` (clean up + restart).
- **Health:** `GET /healthz` (unauthenticated) — poligon, ws-scrcpy, tunnel, adb
  device count, iOS screens ready/total. The dashboard shows it as a dot in the
  top bar.
- **On poligon restart:** iOS screens are torn down and rebuilt (≈1 min each) —
  no orphaned `ios runwda` / `ios forward` processes accumulate. A background
  watchdog also auto-restarts an iOS screen whose WebDriverAgent stops answering.
- **Power loss:** with `scripts/host-setup.sh` applied (FileVault off, auto-login,
  `pmset autorestart 1`) the mac powers on, logs in, and all services come up
  with no human at the keyboard.

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
internal/capture   screenshot / logcat off a device
internal/runner    automated test runs (install_smoke, maestro)
internal/api       JSON API + dashboard
internal/webui     embedded dashboard assets
```
