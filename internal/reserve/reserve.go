// Package reserve implements device booking: one active holder per device,
// heartbeat-renewed leases with an idle timeout and a hard cap.
package reserve

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

var (
	// ErrTaken means the device already has an active reservation.
	ErrTaken = errors.New("device already reserved")
	// ErrNotHolder means the caller does not hold the reservation.
	ErrNotHolder = errors.New("not the reservation holder")
	// ErrUnavailable means the device is offline/degraded/maintenance.
	ErrUnavailable = errors.New("device not available")
)

// Manager coordinates reservations against the store.
type Manager struct {
	st          *store.Store
	db          *sql.DB
	idleTimeout time.Duration
	maxLease    time.Duration
}

// New builds a reservation Manager. db is the same handle store uses.
func New(st *store.Store, db *sql.DB, idleTimeout, maxLease time.Duration) *Manager {
	return &Manager{st: st, db: db, idleTimeout: idleTimeout, maxLease: maxLease}
}

// Reserve gives the device to user, if free.
func (m *Manager) Reserve(deviceID, user string) (model.Reservation, error) {
	d, err := m.st.Device(deviceID)
	if err != nil {
		return model.Reservation{}, err
	}
	switch d.Status {
	case model.StatusFree:
		// ok
	case model.StatusReserved, model.StatusBusy, model.StatusRunningTest:
		return model.Reservation{}, ErrTaken
	default:
		return model.Reservation{}, fmt.Errorf("%w: %s", ErrUnavailable, d.Status)
	}

	now := time.Now()
	res := model.Reservation{
		DeviceID: deviceID, User: user,
		CreatedAt: now, RenewedAt: now, ExpiresAt: now.Add(m.maxLease),
	}
	tx, err := m.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	r, err := tx.Exec(
		`INSERT INTO reservations (device_id, user, created_at, expires_at, renewed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		deviceID, user, now, res.ExpiresAt, now)
	if err != nil {
		return res, ErrTaken // unique partial index violation
	}
	res.ID, _ = r.LastInsertId()
	if _, err := tx.Exec(`UPDATE devices SET status = ? WHERE id = ?`, model.StatusReserved, deviceID); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// Heartbeat renews a lease. Fails if the caller is not the holder.
func (m *Manager) Heartbeat(deviceID, user string) error {
	now := time.Now()
	r, err := m.db.Exec(
		`UPDATE reservations SET renewed_at = ?
		 WHERE device_id = ? AND user = ? AND released = 0`,
		now, deviceID, user)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotHolder
	}
	return nil
}

// Release ends a reservation. admin bypasses the holder check.
func (m *Manager) Release(deviceID, user string, admin bool) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	q := `UPDATE reservations SET released = 1 WHERE device_id = ? AND released = 0`
	args := []any{deviceID}
	if !admin {
		q += ` AND user = ?`
		args = append(args, user)
	}
	r, err := tx.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotHolder
	}
	if _, err := tx.Exec(`UPDATE devices SET status = ? WHERE id = ? AND status IN (?,?,?)`,
		model.StatusFree, deviceID,
		model.StatusReserved, model.StatusBusy, model.StatusRunningTest); err != nil {
		return err
	}
	return tx.Commit()
}

// Holder returns the active reservation for a device, or ok=false.
func (m *Manager) Holder(deviceID string) (model.Reservation, bool, error) {
	var res model.Reservation
	err := m.db.QueryRow(
		`SELECT id, device_id, user, created_at, expires_at, renewed_at
		 FROM reservations WHERE device_id = ? AND released = 0`, deviceID).
		Scan(&res.ID, &res.DeviceID, &res.User, &res.CreatedAt, &res.ExpiresAt, &res.RenewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return res, false, nil
	}
	return res, err == nil, err
}

// ReapExpired releases reservations past their idle timeout or hard cap.
// Call on a ticker.
func (m *Manager) ReapExpired() (int, error) {
	now := time.Now()
	idleCut := now.Add(-m.idleTimeout)
	rows, err := m.db.Query(
		`SELECT device_id, user FROM reservations
		 WHERE released = 0 AND (renewed_at < ? OR expires_at < ?)`,
		idleCut, now)
	if err != nil {
		return 0, err
	}
	var stale [][2]string
	for rows.Next() {
		var dev, user string
		if err := rows.Scan(&dev, &user); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, [2]string{dev, user})
	}
	rows.Close()

	n := 0
	for _, s := range stale {
		if err := m.Release(s[0], s[1], true); err == nil {
			n++
		}
	}
	return n, nil
}
