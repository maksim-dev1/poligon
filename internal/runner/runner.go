// Package runner executes automated test runs on a set of reserved devices.
// A run holds a reservation batch for its lifetime, fans out across its devices
// (parallel, bounded), and writes per-device artifacts under <dir>/<run>/<device>/.
//
// Run types:
//   - install_smoke: install the build, launch it, wait, then assert the process
//     is alive and no crash landed in the log; saves a screenshot + the log.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/capture"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/store"
)

// Types is the set of run types the runner understands.
var Types = map[string]bool{"install_smoke": true}

// Runner schedules and executes runs. One run executes at a time; devices within
// a run run in parallel.
type Runner struct {
	st   *store.Store
	res  *reserve.Manager
	inst *install.Installer
	cap  *capture.Capturer
	adb  *adb.ADB
	log  *slog.Logger

	dir string // artifact root: <StorageDir>/runs
	par int    // max parallel devices per run

	wake    chan struct{}
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// New builds a Runner. dir is created on first use.
func New(st *store.Store, res *reserve.Manager, inst *install.Installer, cap *capture.Capturer, a *adb.ADB, dir string, log *slog.Logger) *Runner {
	return &Runner{
		st: st, res: res, inst: inst, cap: cap, adb: a, log: log,
		dir: dir, par: 4,
		wake:    make(chan struct{}, 1),
		cancels: map[string]context.CancelFunc{},
	}
}

// Submit reserves the devices, records a queued run, and wakes the scheduler.
func (r *Runner) Submit(user, typ string, spec model.RunSpec, deviceIDs []string, trigger string) (model.Run, error) {
	if !Types[typ] {
		return model.Run{}, fmt.Errorf("unknown run type %q", typ)
	}
	seen := map[string]bool{}
	plats := map[string]model.Platform{}
	for _, id := range deviceIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		dev, err := r.st.Device(id)
		if err != nil {
			return model.Run{}, err
		}
		plats[id] = dev.Platform
	}
	if len(plats) == 0 {
		return model.Run{}, fmt.Errorf("no devices")
	}

	ids := make([]string, 0, len(plats))
	for id := range plats {
		ids = append(ids, id)
	}

	// Reuse the caller's existing hold when every device is already theirs
	// (the screen-grid flow); otherwise reserve a fresh batch (the API flow).
	held, free := 0, 0
	for _, id := range ids {
		if res, ok, _ := r.res.Holder(id); ok && res.User == user {
			held++
		} else if ok {
			return model.Run{}, fmt.Errorf("%s is reserved by %s", id, res.User)
		} else {
			free++
		}
	}
	var batch string
	if free > 0 && held == 0 {
		b, _, err := r.res.ReserveMany(ids, user)
		if err != nil {
			return model.Run{}, fmt.Errorf("reserve devices: %w", err)
		}
		batch = b
	} else if free > 0 {
		return model.Run{}, fmt.Errorf("mix of held and free devices — reserve all or none first")
	}
	// batch == "" means the devices are borrowed from the caller's own hold and
	// must not be released when the run ends.

	run := model.Run{
		ID: newID(), User: user, Type: typ, Trigger: trigger,
		Batch: batch, Spec: spec, Status: model.RunQueued,
	}
	if err := r.st.CreateRun(run, plats); err != nil {
		if batch != "" {
			_ = r.res.ReleaseBatch(batch, user, true)
		}
		return model.Run{}, err
	}
	r.signal()
	r.log.Info("run submitted", "run", run.ID, "type", typ, "user", user, "devices", len(ids))
	return r.st.Run(run.ID)
}

// Cancel stops an in-flight run.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[id]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// recover cleans up runs left "running" by a previous process: fail their
// unfinished devices, mark the run errored, release its held batch.
func (r *Runner) recover() {
	orphans, err := r.st.OrphanRuns()
	if err != nil {
		r.log.Warn("runner: scan orphans", "err", err)
		return
	}
	for id, batch := range orphans {
		_ = r.st.FailRunDevicesNotDone(id, "poligon restarted mid-run")
		fin := time.Now()
		_ = r.st.SetRunStatus(id, model.RunError, "poligon restarted mid-run", nil, &fin)
		if owner, e := r.st.RunOwner(id); e == nil && batch != "" {
			_ = r.res.ReleaseBatch(batch, owner, true)
		}
		r.log.Info("runner: recovered orphan run", "run", id)
	}
}

// Run is the scheduler loop. Blocks until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	r.recover()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		r.drainQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-t.C:
		}
	}
}

func (r *Runner) drainQueue(ctx context.Context) {
	for {
		ids, err := r.st.QueuedRunIDs()
		if err != nil {
			r.log.Warn("runner: list queue", "err", err)
			return
		}
		if len(ids) == 0 || ctx.Err() != nil {
			return
		}
		r.execute(ctx, ids[0])
	}
}

func (r *Runner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) execute(parent context.Context, id string) {
	run, err := r.st.Run(id)
	if err != nil {
		r.log.Warn("runner: load run", "run", id, "err", err)
		return
	}
	if run.Status != model.RunQueued {
		return
	}

	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.cancels[id] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
		cancel()
	}()

	now := time.Now()
	_ = r.st.SetRunStatus(id, model.RunRunning, "", &now, nil)

	// keep the reservation batch alive while the run works (only when the run
	// owns the batch; a borrowed hold is the caller's to renew)
	hbStop := make(chan struct{})
	if run.Batch != "" {
		go func() {
			tk := time.NewTicker(45 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-hbStop:
					return
				case <-tk.C:
					_ = r.res.HeartbeatBatch(run.Batch, run.User)
				}
			}
		}()
	}

	sem := make(chan struct{}, r.par)
	var wg sync.WaitGroup
	for i := range run.Devices {
		rd := run.Devices[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.runDevice(ctx, run, rd)
		}()
	}
	wg.Wait()
	close(hbStop)

	// aggregate final status from the freshly-written device rows
	final, err := r.st.Run(id)
	status, detail := model.RunPassed, ""
	if err == nil {
		var pass, fail, errd, skip int
		for _, d := range final.Devices {
			switch d.Status {
			case model.RunPassed:
				pass++
			case model.RunFailed:
				fail++
			case model.RunError:
				errd++
			case model.RunSkipped:
				skip++
			}
		}
		switch {
		case ctx.Err() != nil:
			status, detail = model.RunCanceled, "canceled"
		case errd > 0:
			status, detail = model.RunError, fmt.Sprintf("%d device(s) errored", errd)
		case fail > 0:
			status, detail = model.RunFailed, fmt.Sprintf("%d device(s) failed", fail)
		case pass == 0 && skip > 0:
			status, detail = model.RunError, "every device skipped this run type"
		}
	}
	fin := time.Now()
	_ = r.st.SetRunStatus(id, status, detail, nil, &fin)
	if run.Batch != "" {
		_ = r.res.ReleaseBatch(run.Batch, run.User, true)
	}
	r.log.Info("run finished", "run", id, "status", status)
}

func (r *Runner) runDevice(ctx context.Context, run model.Run, rd model.RunDevice) {
	start := time.Now()
	rd.RunID = run.ID
	rd.StartedAt = &start
	rd.Status = model.RunRunning
	_ = r.st.SetRunDevice(rd)

	defer func() {
		fin := time.Now()
		rd.FinishedAt = &fin
		if rd.Status == model.RunRunning {
			rd.Status = model.RunError
			rd.Detail = "run ended without a verdict"
		}
		_ = r.st.SetRunDevice(rd)
	}()

	dev, err := r.st.Device(rd.DeviceID)
	if err != nil {
		rd.Status, rd.Detail = model.RunError, err.Error()
		return
	}

	devDir := filepath.Join(r.dir, run.ID, rd.DeviceID)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		rd.Status, rd.Detail = model.RunError, err.Error()
		return
	}

	switch run.Type {
	case "install_smoke":
		r.smoke(ctx, run, dev, &rd, devDir)
	default:
		rd.Status, rd.Detail = model.RunSkipped, "unknown run type"
	}
}

// smoke: install → launch → settle → assert alive + no crash.
func (r *Runner) smoke(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	if dev.Platform != model.Android {
		rd.Status, rd.Detail = model.RunSkipped, "install_smoke supports Android only for now"
		return
	}
	art := run.Spec.Artifacts[dev.Platform]
	if art == "" {
		rd.Status, rd.Detail = model.RunSkipped, "no "+string(dev.Platform)+" artifact"
		return
	}

	_ = r.st.SetDeviceStatus(dev.ID, model.StatusRunningTest, time.Now())
	defer r.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())

	_ = r.cap.ClearLogs(ctx, dev)

	res, ierr := r.inst.Run(ctx, dev, art)
	rd.Package = res.Package
	if ierr != nil {
		rd.Status, rd.Detail = model.RunError, "install failed: "+ierr.Error()
		r.saveLog(ctx, dev, rd, devDir)
		return
	}

	watch := run.Spec.WatchSeconds
	if watch <= 0 {
		watch = 8
	}
	select {
	case <-ctx.Done():
		rd.Status, rd.Detail = model.RunCanceled, "canceled"
		return
	case <-time.After(time.Duration(watch) * time.Second):
	}

	if img, mime, err := r.cap.Screenshot(ctx, dev); err == nil {
		name := "screenshot.png"
		if mime == "image/jpeg" {
			name = "screenshot.jpg"
		}
		if os.WriteFile(filepath.Join(devDir, name), img, 0o644) == nil {
			rd.Artifacts = append(rd.Artifacts, name)
		}
	}
	logText := r.saveLog(ctx, dev, rd, devDir)

	alive, _ := r.adb.Running(ctx, dev.Serial, res.Package)
	crash := scanCrash(logText, res.Package)
	switch {
	case crash != "":
		rd.Status, rd.Detail = model.RunFailed, "crash in log: "+crash
	case !alive:
		rd.Status, rd.Detail = model.RunFailed, fmt.Sprintf("process not running %ds after launch", watch)
	default:
		rd.Status, rd.Detail = model.RunPassed, ""
	}
}

func (r *Runner) saveLog(ctx context.Context, dev model.Device, rd *model.RunDevice, devDir string) string {
	text, err := r.cap.Logcat(ctx, dev)
	if err != nil || text == "" {
		return text
	}
	if os.WriteFile(filepath.Join(devDir, "logcat.txt"), []byte(text), 0o644) == nil {
		rd.Artifacts = append(rd.Artifacts, "logcat.txt")
	}
	return text
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
