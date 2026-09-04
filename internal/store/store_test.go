package store

import (
	"path/filepath"
	"testing"

	"github.com/pancir/poligon/internal/model"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestConfigDeviceIsAdopted(t *testing.T) {
	st := openTemp(t)
	if err := st.UpsertDeviceInventory(model.Device{ID: "a1", Platform: model.Android, Serial: "S1"}); err != nil {
		t.Fatal(err)
	}
	d, err := st.Device("a1")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Adopted {
		t.Fatalf("config device should be adopted")
	}
}

func TestAutoDeviceIsCandidate(t *testing.T) {
	st := openTemp(t)
	err := st.CreateAutoDevice(model.Device{
		ID: "pending-S2", Platform: model.Android, Serial: "S2",
		Status: model.StatusUnauthorized, Adopted: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := st.Device("pending-S2")
	if d.Adopted {
		t.Fatalf("auto device should start as a candidate")
	}
	if err := st.SetAdopted("pending-S2", true); err != nil {
		t.Fatal(err)
	}
	d, _ = st.Device("pending-S2")
	if !d.Adopted {
		t.Fatalf("SetAdopted did not stick")
	}
}

func TestRenameDeviceCascades(t *testing.T) {
	st := openTemp(t)
	_ = st.CreateAutoDevice(model.Device{ID: "pending-S3", Platform: model.IOS, UDID: "U3", Status: model.StatusFree})
	if err := st.SetIOSScreen(IOSScreenRow{DeviceID: "pending-S3", WDA: "127.0.0.1:18100", MJPEG: "127.0.0.1:19100"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RenameDevice("pending-S3", "apple-iphone-6s"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Device("apple-iphone-6s"); err != nil {
		t.Fatalf("renamed device missing: %v", err)
	}
	rows, err := st.IOSScreens()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DeviceID != "apple-iphone-6s" {
		t.Fatalf("ios_screen row not cascaded: %+v", rows)
	}
}
