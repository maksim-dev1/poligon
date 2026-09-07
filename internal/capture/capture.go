// Package capture pulls diagnostics off a device during a manual session or a
// test run: a screenshot, the log buffer, (later) a screen recording. It is the
// shared foundation the Phase 3 test runner builds on.
package capture

import (
	"context"
	"fmt"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
)

// Capturer grabs diagnostics from a device.
type Capturer struct {
	adb *adb.ADB
	ios *iosscreen.Controller
}

// New builds a Capturer. ios may be nil on an Android-only farm.
func New(a *adb.ADB, ic *iosscreen.Controller) *Capturer {
	return &Capturer{adb: a, ios: ic}
}

// Screenshot returns image bytes and their MIME type. Android grabs the
// framebuffer directly (PNG); iOS reuses the live-screen frame (JPEG) and so
// needs the device's screen to be up.
func (c *Capturer) Screenshot(ctx context.Context, dev model.Device) ([]byte, string, error) {
	switch dev.Platform {
	case model.Android:
		b, err := c.adb.Screenshot(ctx, dev.Serial)
		return b, "image/png", err
	case model.IOS:
		if c.ios == nil || !c.ios.Configured(dev.ID) {
			return nil, "", fmt.Errorf("iOS live screen is not up for %s", dev.ID)
		}
		b, err := c.ios.Frame(dev.ID)
		return b, "image/jpeg", err
	default:
		return nil, "", fmt.Errorf("screenshot unsupported on %s", dev.Platform)
	}
}

// Logcat returns the device log buffer since the last call and clears it.
// iOS syslog capture is not wired yet.
func (c *Capturer) Logcat(ctx context.Context, dev model.Device) (string, error) {
	switch dev.Platform {
	case model.Android:
		return c.adb.LogcatDump(ctx, dev.Serial)
	case model.IOS:
		return "", fmt.Errorf("iOS syslog capture not implemented yet")
	default:
		return "", fmt.Errorf("logs unsupported on %s", dev.Platform)
	}
}

// ClearLogs empties the device log buffer — call before a test run so a later
// Logcat is scoped to that run. A no-op (nil) on platforms without support.
func (c *Capturer) ClearLogs(ctx context.Context, dev model.Device) error {
	if dev.Platform == model.Android {
		return c.adb.LogcatClear(ctx, dev.Serial)
	}
	return nil
}
