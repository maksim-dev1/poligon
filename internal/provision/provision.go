// Package provision runs the "connect to farm" preparation for a candidate
// device: for Android a quick authorize + specs + rename, for iOS the full
// WebDriverAgent build / run / port-forward pipeline that scripts/ios-wda.sh
// does by hand. Jobs are async; the dashboard polls their progress.
package provision

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

// Job is the live state of one device's preparation.
type Job struct {
	DeviceID  string    `json:"device_id"`
	State     string    `json:"state"` // queued | running | done | failed
	Step      string    `json:"step"`  // human label of the current step
	Log       []string  `json:"log"`
	Err       string    `json:"err,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

const (
	stateRunning = "running"
	stateDone    = "done"
	stateFailed  = "failed"
)

// wdaProc holds the long-lived processes backing one iOS device's screen. They
// are deliberately detached from the server so screens survive a poligon
// restart; Resume re-creates any that died.
type wdaProc struct {
	run       *exec.Cmd // xcodebuild test-without-building
	wda       *exec.Cmd // iproxy <wdaPort>:8100
	mjpeg     *exec.Cmd // iproxy <mjpegPort>:9100
	wdaPort   int
	mjpegPort int
}

// Manager owns adopt jobs and the iOS screen processes.
type Manager struct {
	cfg    config.Config
	st     *store.Store
	adb    *adb.ADB
	ios    ios.Tools
	iosCtl *iosscreen.Controller
	log    *slog.Logger

	mu    sync.Mutex
	jobs  map[string]*Job
	procs map[string]*wdaProc // deviceID -> processes
}

// New wires a Manager.
func New(cfg config.Config, st *store.Store, a *adb.ADB, iosTools ios.Tools, iosCtl *iosscreen.Controller, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, st: st, adb: a, ios: iosTools, iosCtl: iosCtl, log: log,
		jobs:  map[string]*Job{},
		procs: map[string]*wdaProc{},
	}
}

// Get returns a snapshot of a device's job, if one exists.
func (m *Manager) Get(deviceID string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[deviceID]
	if !ok {
		return Job{}, false
	}
	return j.copy(), true
}

// Start kicks off preparation for a candidate device. It returns the initial
// job snapshot; the caller polls Get for progress.
func (m *Manager) Start(deviceID string) (Job, error) {
	d, err := m.st.Device(deviceID)
	if err != nil {
		return Job{}, err
	}
	if d.Adopted {
		return Job{}, fmt.Errorf("%s is already on the farm", deviceID)
	}

	m.mu.Lock()
	if j, ok := m.jobs[deviceID]; ok && j.State == stateRunning {
		snap := j.copy()
		m.mu.Unlock()
		return snap, nil
	}
	j := &Job{DeviceID: deviceID, State: stateRunning, Step: "queued", StartedAt: time.Now()}
	m.jobs[deviceID] = j
	m.mu.Unlock()

	go m.run(d, j)
	return j.copy(), nil
}

func (m *Manager) run(d model.Device, j *Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var err error
	switch d.Platform {
	case model.Android:
		err = m.adoptAndroid(ctx, d, j)
	case model.IOS:
		err = m.adoptIOS(ctx, d, j)
	default:
		err = fmt.Errorf("unknown platform %q", d.Platform)
	}

	m.mu.Lock()
	if err != nil {
		j.State, j.Err = stateFailed, err.Error()
		m.log.Warn("adopt failed", "device", d.ID, "err", err)
	} else {
		j.State, j.Step = stateDone, "ready"
		m.log.Info("device adopted", "device", j.DeviceID)
	}
	m.mu.Unlock()
}

// --- job progress helpers (call without holding m.mu) ---

func (m *Manager) step(j *Job, s string) {
	m.mu.Lock()
	j.Step = s
	j.Log = append(j.Log, time.Now().Format("15:04:05")+"  "+s)
	m.mu.Unlock()
	m.log.Info("adopt step", "device", j.DeviceID, "step", s)
}

func (m *Manager) logf(j *Job, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	m.mu.Lock()
	j.Log = append(j.Log, "    "+line)
	if len(j.Log) > 400 {
		j.Log = j.Log[len(j.Log)-400:]
	}
	m.mu.Unlock()
}

func (j *Job) copy() Job {
	out := *j
	out.Log = append([]string(nil), j.Log...)
	return out
}

// finishRegistration renames a pending-* device to a proper id, marks it
// adopted and free. Shared by both platforms.
func (m *Manager) finishRegistration(j *Job, d model.Device, sp model.Specs, newID string) (string, error) {
	m.step(j, "registering on the farm")
	id := d.ID
	if newID != "" && newID != d.ID {
		if err := m.st.RenameDevice(d.ID, newID); err != nil {
			return "", fmt.Errorf("rename %s -> %s: %w", d.ID, newID, err)
		}
		id = newID
		m.mu.Lock()
		j.DeviceID = newID
		m.mu.Unlock()
	}
	_ = m.st.SetDeviceSpecs(id, sp)
	if err := m.st.SetAdopted(id, true); err != nil {
		return "", err
	}
	_ = m.st.SetDeviceStatus(id, model.StatusFree, time.Now())
	return id, nil
}

// takenIDs returns the set of device ids currently in use, for unique naming.
func (m *Manager) takenIDs() map[string]bool {
	taken := map[string]bool{}
	if pool, err := m.st.Devices(); err == nil {
		for _, d := range pool {
			taken[d.ID] = true
		}
	}
	return taken
}
