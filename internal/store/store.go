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
		INSERT INTO devices (id, platform, serial, udid, tags, source)
		VALUES (?, ?, ?, ?, ?, 'config')
		ON CONFLICT(id) DO UPDATE SET
			platform = excluded.platform,
			serial   = excluded.serial,
			udid     = excluded.udid,
			tags     = excluded.tags,
			source   = 'config'`,
		d.ID, d.Platform, d.Serial, d.UDID, string(tags))
	return err
}

// CreateAutoDevice inserts a device discovered on connect. No-op if a device
// with the same id already exists.
func (s *Store) CreateAutoDevice(d model.Device) error {
	tags, _ := json.Marshal(d.Tags)
	specs, _ := json.Marshal(d.Specs)
	_, err := s.db.Exec(`
		INSERT INTO devices (id, platform, serial, udid, tags, status, source, specs, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, 'auto', ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		d.ID, d.Platform, d.Serial, d.UDID, string(tags), d.Status, string(specs), d.LastSeen)
	return err
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
	rows, err := s.db.Query(`SELECT id, platform, serial, udid, tags, status, source, specs, last_seen FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Device
	for rows.Next() {
		var d model.Device
		var tags, specs string
		var seen sql.NullTime
		if err := rows.Scan(&d.ID, &d.Platform, &d.Serial, &d.UDID, &tags, &d.Status, &d.Source, &specs, &seen); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &d.Tags)
		_ = json.Unmarshal([]byte(specs), &d.Specs)
		if seen.Valid {
			d.LastSeen = seen.Time
		}
		out = append(out, d)
	}
	return out, rows.Err()
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
