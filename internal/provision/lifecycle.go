package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Shutdown kills every WebDriverAgent runner and port-forward poligon started.
// iOS screens are rebuilt from the ios_screen table on the next startup
// (Resume), so a clean stop leaves no orphaned processes or held ports.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	procs := m.procs
	m.procs = map[string]*wdaProc{}
	m.mu.Unlock()

	for _, wp := range procs {
		_ = kill(wp.run)
		_ = kill(wp.wda)
		_ = kill(wp.mjpeg)
	}
	// belt and braces — poligon owns every go-ios helper on this host
	_ = killMatching("ios runwda")
	_ = killMatching("ios forward")
	m.log.Info("provision: stopped all WebDriverAgent processes")
}

// ReapOrphans clears leftovers from a previous poligon (crash, kill -9, a deploy
// that didn't stop cleanly) before Resume respawns the iOS screens. Safe to call
// on every startup.
func (m *Manager) ReapOrphans() {
	_ = killMatching("ios runwda")
	_ = killMatching("ios forward")
	_ = killMatching("xcodebuild.*WebDriverAgent")
	freePortRange(m.cfg.IOSWDA.WDAPortBase, 50)
	freePortRange(m.cfg.IOSWDA.MJPEGPortBase, 50)
	if bin := m.adbBin(); bin != "" {
		_ = exec.Command(bin, "start-server").Run()
	}
	m.log.Info("provision: reaped orphan processes and freed WDA ports")
}

// reservePorts atomically picks a free WDA+MJPEG port pair for a device and
// registers a stub proc holding them, so a concurrent startWDA for another
// device sees them as taken. Replace the stub with the real wdaProc on success;
// call releaseReservation on failure.
func (m *Manager) reservePorts(udid string) (wda, mjpeg int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := map[int]bool{}
	for _, p := range m.procs {
		used[p.wdaPort] = true
		used[p.mjpegPort] = true
	}
	wda = freePort(m.cfg.IOSWDA.WDAPortBase, used)
	used[wda] = true
	mjpeg = freePort(m.cfg.IOSWDA.MJPEGPortBase, used)
	m.procs[udid] = &wdaProc{wdaPort: wda, mjpegPort: mjpeg} // reservation only
	return wda, mjpeg
}

// releaseReservation drops a stub reservation (one with no running processes).
func (m *Manager) releaseReservation(udid string) {
	m.mu.Lock()
	if wp := m.procs[udid]; wp != nil && wp.run == nil {
		delete(m.procs, udid)
	}
	m.mu.Unlock()
}

func (m *Manager) adbBin() string {
	if m.cfg.ADBPath != "" {
		return m.cfg.ADBPath
	}
	return "adb"
}

// freePortRange kills whatever holds each TCP port in [base, base+n).
func freePortRange(base, n int) {
	if base <= 0 {
		return
	}
	for p := base; p < base+n; p++ {
		out, err := exec.Command("lsof", "-ti", fmt.Sprintf("tcp:%d", p), "-sTCP:LISTEN").Output()
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(out)) {
			_ = exec.Command("kill", "-9", pid).Run()
		}
	}
}

// mountDDI makes sure the Developer Disk Image is mounted (iOS 17+ needs it for
// testmanagerd). `ios image auto` is idempotent — a no-op once mounted — and
// reuses a previously downloaded image from ddiDir/devimages.
func (m *Manager) mountDDI(j *Job, udid string) error {
	dir := expandHome(m.cfg.IOSWDA.DDIDir)
	if dir == "" {
		dir = expandHome("~/.cache/poligon/ddi")
	}
	_ = os.MkdirAll(dir, 0o755)
	m.step(j, "mounting Developer Disk Image")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := run(ctx, "ios", "image", "auto", "--basedir", dir, "--udid="+udid)
	if err != nil {
		return fmt.Errorf("mount Developer Disk Image (ios image auto): %w — %s", err, oneLine(out))
	}
	return nil
}

// tunnelReady reports whether the go-ios tunnel agent (com.pancir.go-ios-tunnel)
// is up — required for CoreDevice / iOS 17+.
func tunnelReady() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ios", "tunnel", "ls").Run() == nil
}

// iosMajor returns the device's iOS major version, cached. 0 = unknown.
func (m *Manager) iosMajor(udid string) int {
	m.mu.Lock()
	if m.iosVer == nil {
		m.iosVer = map[string]int{}
	}
	if v, ok := m.iosVer[udid]; ok {
		m.mu.Unlock()
		return v
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ios", "info", "--udid="+udid).Output()
	v := 0
	if err == nil {
		var info struct {
			ProductVersion string `json:"ProductVersion"`
		}
		if json.Unmarshal(out, &info) == nil {
			if i := strings.IndexByte(info.ProductVersion, '.'); i > 0 {
				v, _ = strconv.Atoi(info.ProductVersion[:i])
			} else {
				v, _ = strconv.Atoi(info.ProductVersion)
			}
		}
	}
	if v > 0 {
		m.mu.Lock()
		m.iosVer[udid] = v
		m.mu.Unlock()
	}
	return v
}
