// Package store is poligon's persistence layer: a single SQLite file holding
// device state, users, reservations and install history.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pancir/poligon/internal/model"
)

//go:embed schema.sql
var schema string

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: serialize writers, simplest correct choice
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	migrate(db)
	return &Store{db: db}, nil
}

// migrate applies additive schema changes to pre-existing databases.
// Each statement is best-effort; "duplicate column" on an already-migrated DB is
// expected and ignored.
func migrate(db *sql.DB) {
	for _, stmt := range []string{
		`ALTER TABLE devices ADD COLUMN source TEXT NOT NULL DEFAULT 'config'`,
		`ALTER TABLE reservations ADD COLUMN batch TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = db.Exec(stmt)
	}

	// adopted column: on the first run that adds it, backfill existing rows to
	// adopted=1 so devices that already worked don't regress to "candidate".
	// The backfill is gated on the ALTER succeeding, so it happens exactly once.
	if _, err := db.Exec(`ALTER TABLE devices ADD COLUMN adopted INTEGER NOT NULL DEFAULT 0`); err == nil {
		_, _ = db.Exec(`UPDATE devices SET adopted = 1`)
	}
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for packages that need transactions (e.g. reserve).
func (s *Store) DB() *sql.DB { return s.db }

// UpsertDeviceInventory inserts devices from config, leaving runtime columns
// (status, specs, last_seen) untouched for rows that already exist.
func (s *Store) UpsertDeviceInventory(d model.Device) error {
	tags, _ := json.Marshal(d.Tags)
	_, err := s.db.Exec(`
		INSERT INTO devices (id, platform, serial, udid, tags, source, adopted)
		VALUES (?, ?, ?, ?, ?, 'config', 1)
		ON CONFLICT(id) DO UPDATE SET
			platform = excluded.platform,
			serial   = excluded.serial,
			udid     = excluded.udid,
			tags     = excluded.tags,
			source   = 'config',
			adopted  = 1`,
		d.ID, d.Platform, d.Serial, d.UDID, string(tags))
	return err
}

// CreateAutoDevice inserts a device discovered on connect. No-op if a device
// with the same id already exists.
func (s *Store) CreateAutoDevice(d model.Device) error {
	tags, _ := json.Marshal(d.Tags)
	specs, _ := json.Marshal(d.Specs)
	_, err := s.db.Exec(`
		INSERT INTO devices (id, platform, serial, udid, tags, status, source, specs, last_seen, adopted)
		VALUES (?, ?, ?, ?, ?, ?, 'auto', ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		d.ID, d.Platform, d.Serial, d.UDID, string(tags), d.Status, string(specs), d.LastSeen, b2i(d.Adopted))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SetDeviceStatus updates a device's status and last_seen.
func (s *Store) SetDeviceStatus(id string, st model.DeviceStatus, seen time.Time) error {
	_, err := s.db.Exec(`UPDATE devices SET status = ?, last_seen = ? WHERE id = ?`, st, seen, id)
	return err
}

// DeleteDevice removes a device row (used to upgrade a provisional auto entry).
func (s *Store) DeleteDevice(id string) error {
	_, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	return err
}

// SetDeviceSpecs stores refreshed hardware specs.
func (s *Store) SetDeviceSpecs(id string, sp model.Specs) error {
	b, _ := json.Marshal(sp)
	_, err := s.db.Exec(`UPDATE devices SET specs = ? WHERE id = ?`, string(b), id)
	return err
}

// Devices returns the full pool.
func (s *Store) Devices() ([]model.Device, error) {
	rows, err := s.db.Query(`SELECT id, platform, serial, udid, tags, status, source, specs, last_seen, adopted FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Device
	for rows.Next() {
		var d model.Device
		var tags, specs string
		var seen sql.NullTime
		var adopted int
		if err := rows.Scan(&d.ID, &d.Platform, &d.Serial, &d.UDID, &tags, &d.Status, &d.Source, &specs, &seen, &adopted); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &d.Tags)
		_ = json.Unmarshal([]byte(specs), &d.Specs)
		if seen.Valid {
			d.LastSeen = seen.Time
		}
		d.Adopted = adopted != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetAdopted flips a device's adopted flag.
func (s *Store) SetAdopted(id string, v bool) error {
	_, err := s.db.Exec(`UPDATE devices SET adopted = ? WHERE id = ?`, b2i(v), id)
	return err
}

// RenameDevice changes a device's id, cascading to rows that reference it.
// Used to promote a provisional "pending-*" entry to a proper name.
func (s *Store) RenameDevice(oldID, newID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// children reference devices.id with immediate FKs; defer the check to COMMIT
	// so we can rename the parent and its referrers in one transaction.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	for _, q := range []string{
		`UPDATE devices      SET id = ?        WHERE id = ?`,
		`UPDATE reservations SET device_id = ? WHERE device_id = ?`,
		`UPDATE installs     SET device_id = ? WHERE device_id = ?`,
		`UPDATE ios_screen   SET device_id = ? WHERE device_id = ?`,
	} {
		if _, err := tx.Exec(q, newID, oldID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IOSScreenRow is one persisted iOS live-screen endpoint + its process pids.
type IOSScreenRow struct {
	DeviceID                    string
	WDA, MJPEG                  string
	WDARunPID, WDAPID, MJPEGPID int
}

// IOSScreens returns every persisted iOS screen endpoint.
func (s *Store) IOSScreens() ([]IOSScreenRow, error) {
	rows, err := s.db.Query(`SELECT device_id, wda, mjpeg, wda_run_pid, wda_pid, mjpeg_pid FROM ios_screen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IOSScreenRow
	for rows.Next() {
		var r IOSScreenRow
		if err := rows.Scan(&r.DeviceID, &r.WDA, &r.MJPEG, &r.WDARunPID, &r.WDAPID, &r.MJPEGPID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetIOSScreen upserts a device's iOS screen endpoint + pids.
func (s *Store) SetIOSScreen(r IOSScreenRow) error {
	_, err := s.db.Exec(`
		INSERT INTO ios_screen (device_id, wda, mjpeg, wda_run_pid, wda_pid, mjpeg_pid)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			wda = excluded.wda, mjpeg = excluded.mjpeg,
			wda_run_pid = excluded.wda_run_pid,
			wda_pid = excluded.wda_pid, mjpeg_pid = excluded.mjpeg_pid`,
		r.DeviceID, r.WDA, r.MJPEG, r.WDARunPID, r.WDAPID, r.MJPEGPID)
	return err
}

// Device returns a single device by id.
func (s *Store) Device(id string) (model.Device, error) {
	all, err := s.Devices()
	if err != nil {
		return model.Device{}, err
	}
	for _, d := range all {
		if d.ID == id {
			return d, nil
		}
	}
	return model.Device{}, fmt.Errorf("device %q not found", id)
}
