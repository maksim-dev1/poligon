-- poligon schema. Applied idempotently on startup.

CREATE TABLE IF NOT EXISTS devices (
    id            TEXT PRIMARY KEY,
    platform      TEXT NOT NULL,
    serial        TEXT NOT NULL DEFAULT '',
    udid          TEXT NOT NULL DEFAULT '',
    tags          TEXT NOT NULL DEFAULT '[]',   -- json array
    status        TEXT NOT NULL DEFAULT 'offline',
    source        TEXT NOT NULL DEFAULT 'config', -- config | auto
    specs         TEXT NOT NULL DEFAULT '{}',   -- json Specs
    last_seen     TIMESTAMP,
    adopted       INTEGER NOT NULL DEFAULT 0    -- 1 = pool member, 0 = candidate
);

-- persisted iOS live-screen endpoints (WebDriverAgent + mjpeg, forwarded to the
-- host by iproxy). Written when a device is adopted; reloaded on startup so
-- screens survive a restart. pids let poligon check/respawn the processes.
CREATE TABLE IF NOT EXISTS ios_screen (
    device_id     TEXT PRIMARY KEY REFERENCES devices(id),
    wda           TEXT NOT NULL DEFAULT '',   -- host:port of WDA http api
    mjpeg         TEXT NOT NULL DEFAULT '',   -- host:port of WDA mjpeg server
    wda_run_pid   INTEGER NOT NULL DEFAULT 0, -- xcodebuild test-without-building
    wda_pid       INTEGER NOT NULL DEFAULT 0, -- iproxy wda
    mjpeg_pid     INTEGER NOT NULL DEFAULT 0  -- iproxy mjpeg
);

CREATE TABLE IF NOT EXISTS users (
    name        TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL,
    is_admin    INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS reservations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT NOT NULL REFERENCES devices(id),
    user        TEXT NOT NULL REFERENCES users(name),
    batch       TEXT NOT NULL DEFAULT '',   -- groups reservations taken together
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL,
    renewed_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    released    INTEGER NOT NULL DEFAULT 0
);

-- at most one active reservation per device
CREATE UNIQUE INDEX IF NOT EXISTS idx_res_active_device
    ON reservations(device_id) WHERE released = 0;

CREATE TABLE IF NOT EXISTS installs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT NOT NULL REFERENCES devices(id),
    user        TEXT NOT NULL,
    artifact    TEXT NOT NULL,   -- original filename
    package     TEXT NOT NULL DEFAULT '',
    version     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,   -- ok | failed
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
