// Package devices keeps the device pool's runtime state in sync with reality:
// it polls adb / libimobiledevice for connectivity, refreshes hardware specs,
// and demotes flapping devices to "degraded".
package devices

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/naming"
	"github.com/pancir/poligon/internal/store"
)

// Manager owns the poll loop and exposes the current pool.
type Manager struct {
	cfg config.Config
	st  *store.Store
	adb *adb.ADB
	ios ios.Tools
	log *slog.Logger

	isHeld func(string) bool // active-reservation check (from reserve.Manager)

	mu        sync.Mutex
	flaps     map[string]*flap     // device id -> recent transitions
	heldSince map[string]time.Time // device id -> first offline moment while held
}

// heldOfflineGrace is how long a reserved/busy device may vanish from adb /
// libimobiledevice before it is demoted to offline. iOS in particular drops off
// idevice_id briefly whenever WebDriverAgent (re)launches.
const heldOfflineGrace = 90 * time.Second

type flap struct {
	changes []time.Time
	online  bool
}

// New wires a Manager. Inventory from cfg is written into the store. isHeld
// reports whether a device currently has an active reservation, so a device
// that briefly dropped off USB mid-session returns to "reserved", not "free".
func New(cfg config.Config, st *store.Store, isHeld func(string) bool, log *slog.Logger) (*Manager, error) {
	for _, d := range cfg.Devices {
		if err := st.UpsertDeviceInventory(model.Device{
			ID: d.ID, Platform: d.Platform, Serial: d.Serial, UDID: d.UDID, Tags: d.Tags,
		}); err != nil {
			return nil, err
		}
	}
	if isHeld == nil {
		isHeld = func(string) bool { return false }
	}
	return &Manager{
		cfg:       cfg,
		st:        st,
		adb:       adb.New(cfg.ADBPath),
		ios:       ios.Default(),
		isHeld:    isHeld,
		log:       log,
		flaps:     map[string]*flap{},
		heldSince: map[string]time.Time{},
	}, nil
}

// Run blocks, polling until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	poll := time.NewTicker(m.cfg.PollInterval)
	specs := time.NewTicker(m.cfg.SpecsInterval)
	defer poll.Stop()
	defer specs.Stop()

	m.pollOnce(ctx)
	m.refreshSpecs(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			m.pollOnce(ctx)
		case <-specs.C:
			m.refreshSpecs(ctx)
		}
	}
}

// pollOnce reconciles physical connectivity with stored status and registers
// devices that connect without a config entry.
func (m *Manager) pollOnce(ctx context.Context) {
	androidStates, err := m.adb.Serials(ctx)
	if err != nil {
		m.log.Warn("adb poll failed", "err", err)
		androidStates = map[string]string{}
	}
	iosOnline, err := m.ios.OnlineUDIDs(ctx)
	if err != nil {
		m.log.Warn("ios poll failed", "err", err)
		iosOnline = map[string]bool{}
	}

	pool, err := m.st.Devices()
	if err != nil {
		m.log.Error("read pool", "err", err)
		return
	}

	knownSerial := map[string]bool{}
	knownUDID := map[string]bool{}
	takenID := map[string]bool{}
	for _, d := range pool {
		takenID[d.ID] = true
		if d.Serial != "" {
			knownSerial[d.Serial] = true
		}
		if d.UDID != "" {
			knownUDID[d.UDID] = true
		}
	}

	now := time.Now()
	for _, d := range pool {
		var online, authorized bool
		switch d.Platform {
		case model.Android:
			st, seen := androidStates[d.Serial]
			online = seen
			authorized = st == "device"
		case model.IOS:
			online = iosOnline[d.UDID]
			authorized = online
		}
		next := m.reconcile(d, online, authorized, now)
		if next != d.Status {
			if err := m.st.SetDeviceStatus(d.ID, next, now); err != nil {
				m.log.Error("set status", "device", d.ID, "err", err)
			} else {
				m.log.Info("device status", "device", d.ID, "from", d.Status, "to", next)
			}
		} else if online {
			_ = m.st.SetDeviceStatus(d.ID, next, now) // bump last_seen
		}
		// a provisional "pending-*" auto entry that just authorized: replace it
		// with a properly named one carrying real specs.
		if d.Source == model.SourceAuto && strings.HasPrefix(d.ID, "pending-") && authorized {
			m.upgradePending(ctx, d, takenID)
			delete(takenID, d.ID)
		}
	}

	if !m.cfg.AutoDiscover {
		return
	}
	// auto-register anything connected that we don't track yet
	for serial, state := range androidStates {
		if knownSerial[serial] || state == "offline" {
			continue
		}
		m.autoRegisterAndroid(ctx, serial, state == "device", takenID, now)
		knownSerial[serial] = true
	}
	for udid := range iosOnline {
		if knownUDID[udid] {
			continue
		}
		m.autoRegisterIOS(ctx, udid, takenID, now)
		knownUDID[udid] = true
	}
}

// autoRegisterAndroid adds a freshly seen Android device to the pool.
func (m *Manager) autoRegisterAndroid(ctx context.Context, serial string, authorized bool, taken map[string]bool, now time.Time) {
	d := model.Device{
		Platform: model.Android, Serial: serial,
		Source: model.SourceAuto, Tags: []string{"auto"}, LastSeen: now,
	}
	if !authorized {
		d.ID = "pending-" + naming.ShortSerial(serial)
		d.Status = model.StatusUnauthorized
	} else {
		sp, _ := m.adb.Specs(ctx, serial)
		d.Specs = sp
		d.Status = model.StatusFree
		d.ID = naming.UniqueID(naming.Slug(sp.Manufacturer, sp.Model), taken)
	}
	if err := m.st.CreateAutoDevice(d); err != nil {
		m.log.Warn("auto-register android", "serial", serial, "err", err)
		return
	}
	taken[d.ID] = true
	m.log.Info("auto-registered device", "id", d.ID, "serial", serial, "status", d.Status)
}

// autoRegisterIOS adds a freshly seen iOS device to the pool.
func (m *Manager) autoRegisterIOS(ctx context.Context, udid string, taken map[string]bool, now time.Time) {
	sp, err := m.ios.Specs(ctx, udid)
	if err != nil {
		// device present but not yet trusted / Developer Mode off
		d := model.Device{
			ID: "pending-" + naming.ShortSerial(udid), Platform: model.IOS, UDID: udid,
			Status: model.StatusUnauthorized, Source: model.SourceAuto,
			Tags: []string{"auto"}, LastSeen: now,
		}
		_ = m.st.CreateAutoDevice(d)
		taken[d.ID] = true
		return
	}
	d := model.Device{
		ID:       naming.UniqueID(naming.Slug("apple", sp.Model), taken),
		Platform: model.IOS, UDID: udid, Specs: sp,
		Status: model.StatusFree, Source: model.SourceAuto,
		Tags: []string{"auto"}, LastSeen: now,
	}
	if err := m.st.CreateAutoDevice(d); err != nil {
		m.log.Warn("auto-register ios", "udid", udid, "err", err)
		return
	}
	taken[d.ID] = true
	m.log.Info("auto-registered device", "id", d.ID, "udid", udid)
}

// upgradePending swaps a "pending-*" row for a properly named one once the
// device is authorized and its specs are readable.
func (m *Manager) upgradePending(ctx context.Context, d model.Device, taken map[string]bool) {
	var sp model.Specs
	if d.Platform == model.Android {
		sp, _ = m.adb.Specs(ctx, d.Serial)
	} else {
		sp, _ = m.ios.Specs(ctx, d.UDID)
	}
	vendor := sp.Manufacturer
	if d.Platform == model.IOS {
		vendor = "apple"
	}
	newID := naming.UniqueID(naming.Slug(vendor, sp.Model), taken)
	upgraded := model.Device{
		ID: newID, Platform: d.Platform, Serial: d.Serial, UDID: d.UDID,
		Tags: []string{"auto"}, Status: model.StatusFree,
		Source: model.SourceAuto, Specs: sp, LastSeen: time.Now(),
	}
	if err := m.st.CreateAutoDevice(upgraded); err != nil {
		m.log.Warn("upgrade pending", "id", d.ID, "err", err)
		return
	}
	_ = m.st.DeleteDevice(d.ID)
	taken[newID] = true
	m.log.Info("device authorized", "was", d.ID, "now", newID)
}

// reconcile decides the next status given physical presence, whether the device
// is authorized, user-held states (reserved/busy/running) and flap detection.
func (m *Manager) reconcile(d model.Device, online, authorized bool, now time.Time) model.DeviceStatus {
	if m.isFlapping(d.ID, online, now) {
		return model.StatusDegraded
	}
	switch d.Status {
	case model.StatusMaintenance:
		return model.StatusMaintenance // only cleared manually
	case model.StatusReserved, model.StatusBusy, model.StatusRunningTest:
		if online {
			m.clearHeldSince(d.ID)
			return d.Status
		}
		// tolerate a brief disappearance (USB blip, WDA relaunch) — only demote
		// once the device has been gone past the grace window. Real expiry is
		// still handled by the reservation heartbeat / idle timeout.
		if m.heldOfflineTooLong(d.ID, now) {
			return model.StatusOffline
		}
		return d.Status
	case model.StatusDegraded:
		if online && authorized {
			return model.StatusFree
		}
		return model.StatusDegraded
	default:
		switch {
		case online && authorized:
			if m.isHeld(d.ID) {
				return model.StatusReserved // device came back and the lease is still alive
			}
			return model.StatusFree
		case online:
			return model.StatusUnauthorized
		default:
			return model.StatusOffline
		}
	}
}

// heldOfflineTooLong records when a held device first went missing and reports
// whether that was more than heldOfflineGrace ago.
func (m *Manager) heldOfflineTooLong(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	since, ok := m.heldSince[id]
	if !ok {
		m.heldSince[id] = now
		return false
	}
	return now.Sub(since) > heldOfflineGrace
}

func (m *Manager) clearHeldSince(id string) {
	m.mu.Lock()
	delete(m.heldSince, id)
	m.mu.Unlock()
}

// isFlapping reports whether a device changed connectivity 3+ times in 60s.
func (m *Manager) isFlapping(id string, online bool, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.flaps[id]
	if f == nil {
		f = &flap{online: online}
		m.flaps[id] = f
	}
	if f.online != online {
		f.online = online
		f.changes = append(f.changes, now)
	}
	cutoff := now.Add(-60 * time.Second)
	kept := f.changes[:0]
	for _, t := range f.changes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	f.changes = kept
	return len(f.changes) >= 3
}

func (m *Manager) refreshSpecs(ctx context.Context) {
	pool, err := m.st.Devices()
	if err != nil {
		return
	}
	for _, d := range pool {
		var sp model.Specs
		var serr error
		switch d.Platform {
		case model.Android:
			sp, serr = m.adb.Specs(ctx, d.Serial)
		case model.IOS:
			sp, serr = m.ios.Specs(ctx, d.UDID)
		}
		if serr != nil {
			continue // device probably offline; try next cycle
		}
		if err := m.st.SetDeviceSpecs(d.ID, sp); err != nil {
			m.log.Warn("save specs", "device", d.ID, "err", err)
		}
	}
}
