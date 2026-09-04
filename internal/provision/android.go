package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/naming"
)

// adoptAndroid brings a candidate Android device onto the farm: make sure adb
// is authorized, read specs, give it a proper id.
func (m *Manager) adoptAndroid(ctx context.Context, d model.Device, j *Job) error {
	if d.Serial == "" {
		return fmt.Errorf("device has no adb serial")
	}

	m.step(j, "connecting over adb")
	_ = m.adb.Reconnect(ctx, d.Serial)

	deadline := time.Now().Add(120 * time.Second)
	for {
		states, err := m.adb.Serials(ctx)
		if err == nil {
			switch states[d.Serial] {
			case "device":
				goto authorized
			case "unauthorized":
				m.step(j, "waiting: tap \"Allow USB debugging\" (Always allow) on the phone")
			case "":
				m.step(j, "waiting: device not visible to adb — replug the cable")
			default:
				m.logf(j, "adb state: %s", states[d.Serial])
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device did not become authorized in adb within 120s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}

authorized:
	m.step(j, "reading device specs")
	sp, err := m.adb.Specs(ctx, d.Serial)
	if err != nil {
		return fmt.Errorf("read specs: %w", err)
	}
	m.logf(j, "%s %s · Android %s · %s",
		sp.Manufacturer, sp.Model, sp.OSVersion, strings.TrimSpace(sp.SoC))

	newID := d.ID
	if strings.HasPrefix(d.ID, "pending-") {
		newID = naming.UniqueID(naming.Slug(sp.Manufacturer, sp.Model), m.takenIDs())
	}
	if _, err := m.finishRegistration(j, d, sp, newID); err != nil {
		return err
	}
	return nil
}
