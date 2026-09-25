package reserve

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

func newManager(t *testing.T, devs map[string]model.DeviceStatus) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for id, status := range devs {
		if err := st.UpsertDeviceInventory(model.Device{ID: id, Platform: model.Android, Serial: id}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetDeviceStatus(id, status, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{"u@x", "first@x", "second@x"} {
		if err := st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, st.DB(), 15*time.Minute, 4*time.Hour)
}

// An offline device in a batch is unavailable, not "already reserved" —
// the same distinction the single-device Reserve makes.
func TestReserveManyOfflineIsUnavailable(t *testing.T) {
	m := newManager(t, map[string]model.DeviceStatus{"a": model.StatusFree, "b": model.StatusOffline})
	_, _, err := m.ReserveMany([]string{"a", "b"}, "u@x")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrTaken) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestReserveManyHeldIsTaken(t *testing.T) {
	m := newManager(t, map[string]model.DeviceStatus{"a": model.StatusFree})
	if _, err := m.Reserve("a", "first@x"); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.ReserveMany([]string{"a"}, "second@x")
	if !errors.Is(err, ErrTaken) {
		t.Fatalf("want ErrTaken, got %v", err)
	}
}

// A single-device reservation (REST /devices/{id}/reserve, MCP reserve_device)
// must show up as a batch, or the dashboard cannot open its screen.
func TestReserveIsABatchOfOne(t *testing.T) {
	m := newManager(t, map[string]model.DeviceStatus{"a": model.StatusFree})
	if _, err := m.Reserve("a", "u@x"); err != nil {
		t.Fatal(err)
	}
	bs, err := m.UserBatches("u@x")
	if err != nil || len(bs) != 1 {
		t.Fatalf("batches: %+v, %v", bs, err)
	}
	ids, err := m.BatchDevices(bs[0].Batch, "u@x")
	if err != nil || len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("batch devices: %v, %v", ids, err)
	}
}

// "mine" spans every reservation a user holds, across batches, and nobody
// else's.
func TestMineSpansBatches(t *testing.T) {
	m := newManager(t, map[string]model.DeviceStatus{"a": model.StatusFree, "b": model.StatusFree, "c": model.StatusFree})
	if _, err := m.Reserve("a", "u@x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ReserveMany([]string{"b"}, "u@x"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve("c", "first@x"); err != nil {
		t.Fatal(err)
	}
	ids, err := m.BatchDevices(Mine, "u@x")
	if err != nil || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("mine: %v, %v", ids, err)
	}
	if err := m.HeartbeatBatch(Mine, "u@x"); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseBatch(Mine, "u@x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BatchDevices(Mine, "u@x"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("after release: %v", err)
	}
	if _, ok, _ := m.Holder("c"); !ok {
		t.Fatal("another user's device was released")
	}
}
