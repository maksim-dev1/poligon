# poligon — interface system

Established 2026-09-07. All dashboard UI lives in `internal/webui/`, embedded in
the Go binary and served over plain HTTP on the LAN. **Offline-only: system
fonts, no CDN, no webfonts.** One shared stylesheet: `internal/webui/app.css`,
served at `/app.css`. Pages: `index.html` (dashboard), `grid.html` (screen
wall), `ios-screen.html` (iOS player), `setup.html` (set-password).

## Direction & feel

Instrument-grade device lab. Cool near-black, a single blue accent, status as a
physical **LED dot** (not a pill), all technical data (device ids, serials,
specs, logs, counts) in **monospace** like asset tags. The mirrored phone
screens are the only bright thing — chrome recedes.

## Depth

**Borders + tonal surface shifts only. No shadows for elevation** (dark mode
doesn't carry them). Shadows appear only on genuinely-floating layers (tray,
toast, modal) and are neutral black, never colored. **No colored glow / halo**
anywhere — it reads as AI-slop.

## Tokens (see `:root` in app.css)

- Surfaces, one hue, lightness steps: `--bg #0d0f13` → `--surface #14171d` →
  `--surface-2 #191d25`; `--inset #0a0c10` (darker = recessed, for inputs/logs).
- Text, 4 levels: `--text #e8eaed` / `--text-2 #aeb5bf` / `--text-3 #8f96a1` /
  `--text-muted #767d88`.
- Borders: `--line rgba(255,255,255,.07)` / `--line-2 .12` / `--line-strong .18`.
- Accent (only non-neutral): `--accent #2570c4` (dark enough for white text ~5:1),
  `--accent-press #1f5fa8` (button hover = darker), `--accent-hi #5a9fe6` (lighter
  tint for links + focus ring on dark only).
- Status LEDs: `--ok #3fb98a` free/online · `--warn #d99a2b` reserved/busy ·
  `--down #e5544b` offline/error · `--info #5b8def` candidate.

## Type

`--sans` = system-ui stack. `--mono` = `ui-monospace, "SF Mono", "JetBrains
Mono", Menlo, …`. Base **13px** (dense tool — deliberate, per interface-design
skill's dense-UI guidance). Hierarchy via weight + `--text-*` level, not size.
Mono for every technical value; sans for prose/labels/buttons.

## Spacing / radius

Base 4px. Card padding 14px, grid gap 12px, section gap ~26px, topbar 48px.
Radius scale: `--r-xs 4` (checkbox/tag) · `--r-sm 5` (inputs/buttons) ·
`--r-md 9` (cards/tiles) · `--r-lg 12` (tray/modal/auth-card). Concentric.

## Component patterns

- `.btn` — 30px h · 0 12px pad · 5px radius · 12.5px/500. `--primary` (accent),
  `--danger` (red text/border), `--ghost`, `--sm` (26px). `:active` scale .98.
- `.input` — 34px h · inset bg · focus = accent border + `inset 0 0 0 1px accent`.
- `.led` — 7px dot + lowercase mono label; class `led--<status>`. Transient
  states (reserved/running/busy) pulse (respects reduced-motion).
- `.card` — device card. `.card--free` is "lit" (crisp `--line-2` border + a
  1px `--ok` gradient hairline on top). `.card--muted` (opacity .62) for
  offline/degraded. `.card--selected` = accent border + `inset 0 0 0 1px accent`.
- `.specs` — mono key/value rows, values right-aligned `tabular-nums`.
- `.tray` — floating pill, bottom-center, for selection/batch actions.
- `.toast` (`#toasts` container) — replaces `alert()`; left border in
  ok/err/accent. `.modal` — replaces `prompt()`; centered, `scale(.97)` in.
- `.wall` / `.tile` — screen grid: 8px gutter, 28px mono chrome strip, iframe fills.

## Layout patterns (2026-09-09)

UI language is **Russian**. Keep technical tokens verbatim (device ids,
`.apk/.ipa`, MSISDN, MIUI, USB, WebDriverAgent, Maestro, logcat, adb).

- **`.page-title`** — real 20px/600 page heading (`Мобильные устройства`),
  replaces the tiny uppercase section label as the primary heading.
- **`.filterbar`** — bordered `--surface` panel above the farm grid: search
  (`.input.filter-search`), `.checkbox` per platform, `.select` OS-version
  (options built from live `specs.os_version`), `.switch` `Только свободные`,
  `.viewtoggle` on the right. All filtering is client-side over the last
  `/api/devices` fetch — no extra round-trip. State in `filters` + localStorage
  (`poligon_view`).
- **`.checkbox` / `.switch` / `.select`** — form controls matching `.input`
  metrics; switch turns `--ok` green when on, knob is a pseudo-element.
- **`.icon-btn`** — shared 28px square icon button (rail, pill, viewtoggle,
  tile-bar). Icons are inline `<svg stroke="currentColor">`, 16px, no files.
- **`.grid--list`** — list view for the device grid; pure CSS over the same
  card DOM (horizontal row, `.specs` collapses to an inline strip).
- **`.wall-pill`** — screen wall's single floating control bar, top-center:
  zoom −/%/+ (`--tile-scale` on `.wall`, persisted in `poligon_zoom`),
  `Запуск ▾` menu (`.wall-menu-pop`) holding the run actions, health dot,
  `Ферма`, `Завершить работу`. The old text-button topbar is gone.
- **`.tile-rail`** — per-tile vertical icon column (screenshot / logcat /
  pop-out), hidden until tile hover. **`.tile-info`** — spec popover toggled
  by the `i` button in the slim `.tile-bar` (`×` · id · `⇄` reopen · `i`).

## Motion

< 200ms, `--ease = cubic-bezier(0.23,1,0.32,1)`. No entrance animation on the
device list (polls every 5s — would flicker). Button `:active` scale only in the
hot path.

## Sanctioned exceptions to the impeccable design hook

- **13px body / 11–11.5px mono metadata** — deliberate density for a technical
  lab tool; smallest text is tertiary asset-tag / helper copy.
- Small mono labels on the darkest surfaces sit near the 4.5:1 line — accepted
  for `--text-muted`-tier metadata only; all primary/secondary text passes AA.
