// Package capture pulls diagnostics off a device during a manual session or a
// test run: a screenshot, the log buffer, (later) a screen recording. It is the
// shared foundation the Phase 3 test runner builds on.
package capture

import (
	"context"
	"fmt"
	"os/exec"

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

// Keyevent presses a hardware/navigation key — the on-screen bar equivalent of
// power/volume/back/home/recents. Android only.
func (c *Capturer) Keyevent(ctx context.Context, dev model.Device, code int) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("hardware keys unsupported on %s", dev.Platform)
	}
	return c.adb.Keyevent(ctx, dev.Serial, code)
}

// ListFiles lists one directory on the device (Android only).
func (c *Capturer) ListFiles(ctx context.Context, dev model.Device, path string) ([]adb.FileEntry, error) {
	if dev.Platform != model.Android {
		return nil, fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.ListDir(ctx, dev.Serial, path)
}

// PullFile copies a device file to a local path.
func (c *Capturer) PullFile(ctx context.Context, dev model.Device, remote, local string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Pull(ctx, dev.Serial, remote, local)
}

// PushFile copies a local file to a device path.
func (c *Capturer) PushFile(ctx context.Context, dev model.Device, local, remote string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Push(ctx, dev.Serial, local, remote)
}

// RemovePath deletes a file or directory on the device.
func (c *Capturer) RemovePath(ctx context.Context, dev model.Device, path string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Remove(ctx, dev.Serial, path)
}

// RenamePath moves/renames a device path.
func (c *Capturer) RenamePath(ctx context.Context, dev model.Device, from, to string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Rename(ctx, dev.Serial, from, to)
}

// ShellCommand returns an unstarted interactive shell process for a caller to
// wire stdio to (Android only — iOS has no equivalent without a jailbreak).
func (c *Capturer) ShellCommand(ctx context.Context, dev model.Device) (*exec.Cmd, error) {
	if dev.Platform != model.Android {
		return nil, fmt.Errorf("shell unsupported on %s", dev.Platform)
	}
	return c.adb.ShellCommand(ctx, dev.Serial), nil
}

// ClearLogs empties the device log buffer — call before a test run so a later
// Logcat is scoped to that run. A no-op (nil) on platforms without support.
func (c *Capturer) ClearLogs(ctx context.Context, dev model.Device) error {
	if dev.Platform == model.Android {
		return c.adb.LogcatClear(ctx, dev.Serial)
	}
	return nil
}
