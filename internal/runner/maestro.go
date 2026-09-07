package runner

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// runMaestro installs the build (if one was supplied) and runs a Maestro flow
// against the device. Maestro auto-detects the platform from --device, so the
// same flow drives Android and iOS.
func (r *Runner) runMaestro(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	flow := run.Spec.FlowPath
	if flow == "" {
		rd.Status, rd.Detail = model.RunError, "no flow file"
		return
	}

	_ = r.st.SetDeviceStatus(dev.ID, model.StatusRunningTest, time.Now())
	defer r.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())

	if art := run.Spec.Artifacts[dev.Platform]; art != "" {
		_ = r.cap.ClearLogs(ctx, dev)
		if _, ierr := r.inst.Run(ctx, dev, art); ierr != nil {
			rd.Status, rd.Detail = model.RunError, "install failed: "+ierr.Error()
			r.saveLog(ctx, dev, rd, devDir)
			return
		}
	}

	target := dev.Serial
	if dev.Platform == model.IOS {
		target = dev.UDID
	}
	report := filepath.Join(devDir, "report.xml")
	debug := filepath.Join(devDir, "maestro")

	cmd := exec.CommandContext(ctx, r.maestro,
		"--device", target,
		"test", "--format", "junit", "--output", report,
		"--debug-output", debug,
		flow)
	cmd.Env = append(os.Environ(), "MAESTRO_CLI_NO_ANALYTICS=1", "CI=true")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	runErr := cmd.Run()

	if os.WriteFile(filepath.Join(devDir, "maestro.log"), buf.Bytes(), 0o644) == nil {
		rd.Artifacts = append(rd.Artifacts, "maestro.log")
	}
	if _, err := os.Stat(report); err == nil {
		rd.Artifacts = append(rd.Artifacts, "report.xml")
	}
	// pull in whatever maestro wrote (recording.mp4, screenshots, ...)
	_ = filepath.WalkDir(debug, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, e := filepath.Rel(devDir, p); e == nil {
			rd.Artifacts = append(rd.Artifacts, filepath.ToSlash(rel))
		}
		return nil
	})

	if img, mime, err := r.cap.Screenshot(ctx, dev); err == nil {
		name := "screenshot.png"
		if mime == "image/jpeg" {
			name = "screenshot.jpg"
		}
		if os.WriteFile(filepath.Join(devDir, name), img, 0o644) == nil {
			rd.Artifacts = append(rd.Artifacts, name)
		}
	}
	r.saveLog(ctx, dev, rd, devDir)

	switch {
	case ctx.Err() != nil:
		rd.Status, rd.Detail = model.RunCanceled, "canceled"
	case runErr != nil:
		rd.Status, rd.Detail = model.RunFailed, "maestro: "+tail(buf.String(), 400)
	default:
		rd.Status, rd.Detail = model.RunPassed, ""
	}
}

// resolveMaestro finds the maestro binary: PATH first, then the default
// install location (~/.maestro/bin), else the bare name so the error is clear.
func resolveMaestro() string {
	if p, err := exec.LookPath("maestro"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".maestro", "bin", "maestro")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "maestro"
}

// tail returns the last n characters of s, trimmed, prefixed with an ellipsis
// when it was cut.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
