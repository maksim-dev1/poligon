package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// CreateRun inserts a run and one run_devices row per device, all in a
// transaction. The run starts queued; devices start pending.
func (s *Store) CreateRun(r model.Run, deviceIDs map[string]model.Platform) error {
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO runs (id, user, type, status, trigger, batch, spec, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.User, r.Type, string(model.RunQueued), r.Trigger, r.Batch, string(spec), time.Now()); err != nil {
		return err
	}
	for id, plat := range deviceIDs {
		if _, err := tx.Exec(
			`INSERT INTO run_devices (run_id, device_id, platform, status)
			 VALUES (?, ?, ?, ?)`,
			r.ID, id, string(plat), string(model.RunPending)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Run loads one run with its devices.
func (s *Store) Run(id string) (model.Run, error) {
	var (
		r        model.Run
		spec     string
		started  sql.NullTime
		finished sql.NullTime
	)
	err := s.db.QueryRow(
		`SELECT id, user, type, status, trigger, batch, spec, detail, created_at, started_at, finished_at
		 FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.User, &r.Type, &r.Status, &r.Trigger, &r.Batch, &spec, &r.Detail,
			&r.CreatedAt, &started, &finished)
	if err != nil {
		return model.Run{}, err
	}
	_ = json.Unmarshal([]byte(spec), &r.Spec)
	if started.Valid {
		r.StartedAt = &started.Time
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	r.Devices, err = s.runDevices(id)
	return r, err
}

func (s *Store) runDevices(runID string) ([]model.RunDevice, error) {
	rows, err := s.db.Query(
		`SELECT device_id, platform, status, detail, package, artifacts, started_at, finished_at
		 FROM run_devices WHERE run_id = ? ORDER BY device_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RunDevice
	for rows.Next() {
		var (
			d        model.RunDevice
			arts     string
			started  sql.NullTime
			finished sql.NullTime
		)
		if err := rows.Scan(&d.DeviceID, &d.Platform, &d.Status, &d.Detail, &d.Package,
			&arts, &started, &finished); err != nil {
			return nil, err
		}
		d.RunID = runID
		_ = json.Unmarshal([]byte(arts), &d.Artifacts)
		if d.Artifacts == nil {
			d.Artifacts = []string{}
		}
		if started.Valid {
			d.StartedAt = &started.Time
		}
		if finished.Valid {
			d.FinishedAt = &finished.Time
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Runs lists recent runs (newest first), each with its devices.
func (s *Store) Runs(limit int) ([]model.Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id FROM runs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]model.Run, 0, len(ids))
	for _, id := range ids {
		r, err := s.Run(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// QueuedRunIDs returns queued run ids, oldest first — the scheduler's work list.
func (s *Store) QueuedRunIDs() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM runs WHERE status = ? ORDER BY created_at ASC`, string(model.RunQueued))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetRunStatus updates a run's status and (when non-zero) its timestamps.
func (s *Store) SetRunStatus(id string, st model.RunStatus, detail string, started, finished *time.Time) error {
	_, err := s.db.Exec(
		`UPDATE runs SET status = ?, detail = ?,
		   started_at  = COALESCE(?, started_at),
		   finished_at = COALESCE(?, finished_at)
		 WHERE id = ?`,
		string(st), detail, nullTime(started), nullTime(finished), id)
	return err
}

// SetRunDevice updates one device row of a run.
func (s *Store) SetRunDevice(d model.RunDevice) error {
	arts, _ := json.Marshal(d.Artifacts)
	_, err := s.db.Exec(
		`UPDATE run_devices SET status = ?, detail = ?, package = ?, artifacts = ?,
		   started_at  = COALESCE(?, started_at),
		   finished_at = COALESCE(?, finished_at)
		 WHERE run_id = ? AND device_id = ?`,
		string(d.Status), d.Detail, d.Package, string(arts),
		nullTime(d.StartedAt), nullTime(d.FinishedAt), d.RunID, d.DeviceID)
	return err
}

// OrphanRuns returns runs left in a non-terminal state (a crash mid-run) with
// their reservation batch, so the runner can clean them up on startup.
func (s *Store) OrphanRuns() (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT id, batch FROM runs WHERE status = ?`, string(model.RunRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, batch string
		if err := rows.Scan(&id, &batch); err != nil {
			return nil, err
		}
		out[id] = batch
	}
	return out, rows.Err()
}

// FailRunDevicesNotDone marks any still-pending/running device rows of a run as
// errored — used when recovering an orphaned run.
func (s *Store) FailRunDevicesNotDone(runID, detail string) error {
	_, err := s.db.Exec(
		`UPDATE run_devices SET status = ?, detail = ?
		 WHERE run_id = ? AND status IN (?, ?)`,
		string(model.RunError), detail, runID,
		string(model.RunPending), string(model.RunRunning))
	return err
}

// RunOwner returns the run's owner, or an error if the run is unknown.
func (s *Store) RunOwner(id string) (string, error) {
	var u string
	err := s.db.QueryRow(`SELECT user FROM runs WHERE id = ?`, id).Scan(&u)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("run %q not found", id)
	}
	return u, err
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
