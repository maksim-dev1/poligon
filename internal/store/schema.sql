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

-- Farm accounts. Registration is open (anyone who can reach poligon signs up
-- with an email + password); every account is equal, there is no admin role.
CREATE TABLE IF NOT EXISTS users (
    name          TEXT PRIMARY KEY,          -- login id (email address)
    token_hash    TEXT NOT NULL DEFAULT '',  -- legacy bearer token; unused by new logins
    disabled      INTEGER NOT NULL DEFAULT 0,
    pass_hash     TEXT NOT NULL DEFAULT '',  -- bcrypt
    pass_set      INTEGER NOT NULL DEFAULT 0,-- 0 = account created, password not set yet
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- browser login sessions. token_hash = sha256(random 32 bytes); the raw token
-- is only ever in the client cookie. Looked up by the unique index, not scanned.
CREATE TABLE IF NOT EXISTS sessions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_name   TEXT NOT NULL REFERENCES users(name),
    token_hash  TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL,
    ip          TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT '',
    revoked     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_name);

-- one-time "set your password" links handed out by "poligon user add".
CREATE TABLE IF NOT EXISTS enroll_tokens (
    token_hash  TEXT PRIMARY KEY,
    user_name   TEXT NOT NULL REFERENCES users(name),
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL,
    used_at     TIMESTAMP
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
