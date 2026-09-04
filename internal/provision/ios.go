package provision

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/naming"
	"github.com/pancir/poligon/internal/store"
)

const wdaServerLine = "ServerURLHere->"

// adoptIOS runs the full WebDriverAgent pipeline for a candidate iOS device.
func (m *Manager) adoptIOS(ctx context.Context, d model.Device, j *Job) error {
	if d.UDID == "" {
		return fmt.Errorf("device has no udid")
	}
	team := m.wdaTeam()
	if team == "" {
		return fmt.Errorf("iOS provisioning not configured: set ios_wda.team (or $POLIGON_WDA_TEAM) to your Apple team id")
	}
	src := expandHome(m.cfg.IOSWDA.Src)
	dd := expandHome(m.cfg.IOSWDA.DerivedData)
	bundle := m.cfg.IOSWDA.BundleID

	m.step(j, "pairing with the device")
	if out, err := run(ctx, "idevicepair", "-u", d.UDID, "validate"); err != nil {
		return fmt.Errorf("idevicepair validate: %w (%s) — unlock the iPhone and tap Trust", err, oneLine(out))
	}
	sp, err := m.ios.Specs(ctx, d.UDID)
	if err != nil {
		return fmt.Errorf("read specs: %w", err)
	}
	m.logf(j, "%s · iOS %s (%s)", sp.Model, sp.OSVersion, sp.Build)

	// 1. WebDriverAgent checkout
	if _, err := os.Stat(filepath.Join(src, "WebDriverAgent.xcodeproj")); err != nil {
		m.step(j, "fetching WebDriverAgent")
		if out, err := run(ctx, "git", "clone", "--depth", "1",
			"https://github.com/appium/WebDriverAgent.git", src); err != nil {
			return fmt.Errorf("git clone WebDriverAgent: %w (%s)", err, oneLine(out))
		}
	}

	// 2. build-for-testing (shared by every device; skip if already built)
	xctestrun := findXCTestRun(dd)
	if xctestrun == "" {
		m.step(j, "building WebDriverAgent (a few minutes)")
		bctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		err := m.streamCmd(j, exec.CommandContext(bctx, "xcodebuild",
			"build-for-testing",
			"-project", filepath.Join(src, "WebDriverAgent.xcodeproj"),
			"-scheme", "WebDriverAgentRunner",
			"-destination", "generic/platform=iOS",
			"-allowProvisioningUpdates",
			"-derivedDataPath", dd,
			"DEVELOPMENT_TEAM="+team,
			"CODE_SIGN_STYLE=Automatic",
			"PRODUCT_BUNDLE_IDENTIFIER="+bundle,
		))
		cancel()
		if err != nil {
			return fmt.Errorf("xcodebuild build-for-testing: %w", err)
		}
		if xctestrun = findXCTestRun(dd); xctestrun == "" {
			return fmt.Errorf("build produced no .xctestrun under %s", dd)
		}
	}
	m.logf(j, "xctestrun: %s", xctestrun)

	// 3. run WDA + forward ports
	ep, wp, err := m.startWDA(j, d.UDID, xctestrun)
	if err != nil {
		return err
	}

	// 4. register
	m.iosCtl.Set(d.ID, ep)
	newID := d.ID
	if strings.HasPrefix(d.ID, "pending-") {
		newID = naming.UniqueID(naming.Slug("apple", sp.Model), m.takenIDs())
	}
	id, err := m.finishRegistration(j, d, sp, newID)
	if err != nil {
		return err
	}
	if newID != d.ID {
		m.iosCtl.Set(id, ep) // re-key under the final id
		m.mu.Lock()
		m.procs[id] = m.procs[d.UDID]
		m.mu.Unlock()
	}
	_ = m.st.SetIOSScreen(store.IOSScreenRow{
		DeviceID: id, WDA: ep.WDA, MJPEG: ep.MJPEG,
		WDARunPID: pidOf(wp.run), WDAPID: pidOf(wp.wda), MJPEGPID: pidOf(wp.mjpeg),
	})
	return nil
}

// startWDA launches the WDA test runner + two iproxy tunnels for a udid and
// returns the resulting endpoint. Processes are detached (survive a restart).
func (m *Manager) startWDA(j *Job, udid, xctestrun string) (iosscreen.Endpoint, *wdaProc, error) {
	used := m.usedPorts()
	wdaPort := freePort(m.cfg.IOSWDA.WDAPortBase, used)
	used[wdaPort] = true
	mjpegPort := freePort(m.cfg.IOSWDA.MJPEGPortBase, used)

	m.step(j, "starting WebDriverAgent on the device")
	runCmd := exec.Command("xcodebuild", "test-without-building",
		"-xctestrun", xctestrun,
		"-destination", "platform=iOS,id="+udid)
	detach(runCmd)
	if err := m.spawnAndWaitForLine(j, runCmd, wdaServerLine, 180*time.Second); err != nil {
		_ = kill(runCmd)
		return iosscreen.Endpoint{}, nil, fmt.Errorf("WebDriverAgent did not start: %w", err)
	}
	reap(runCmd)

	m.step(j, fmt.Sprintf("forwarding ports (wda:%d mjpeg:%d)", wdaPort, mjpegPort))
	_ = killMatching("iproxy .*" + udid)
	wdaTun := exec.Command("iproxy", fmt.Sprintf("%d:8100", wdaPort), "-u", udid)
	mjpegTun := exec.Command("iproxy", fmt.Sprintf("%d:9100", mjpegPort), "-u", udid)
	detach(wdaTun)
	detach(mjpegTun)
	if err := wdaTun.Start(); err != nil {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("iproxy wda: %w", err)
	}
	if err := mjpegTun.Start(); err != nil {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("iproxy mjpeg: %w", err)
	}
	reap(wdaTun)
	reap(mjpegTun)

	if err := waitFor(func() bool { return probeWDA(wdaPort) == nil }, 30*time.Second); err != nil {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("WDA /status not reachable on :%d", wdaPort)
	}

	wp := &wdaProc{run: runCmd, wda: wdaTun, mjpeg: mjpegTun, wdaPort: wdaPort, mjpegPort: mjpegPort}
	m.mu.Lock()
	m.procs[udid] = wp
	m.mu.Unlock()
	ep := iosscreen.Endpoint{
		WDA:   fmt.Sprintf("127.0.0.1:%d", wdaPort),
		MJPEG: fmt.Sprintf("127.0.0.1:%d", mjpegPort),
	}
	m.logf(j, "WebDriverAgent ready at %s", ep.WDA)
	return ep, wp, nil
}

// Resume reloads persisted iOS screen endpoints on startup and respawns the
// WebDriverAgent/iproxy processes for any whose port is no longer answering.
func (m *Manager) Resume(ctx context.Context) {
	rows, err := m.st.IOSScreens()
	if err != nil {
		m.log.Warn("provision resume: read ios_screen", "err", err)
		return
	}
	xctestrun := findXCTestRun(expandHome(m.cfg.IOSWDA.DerivedData))
	for _, r := range rows {
		ep := iosscreen.Endpoint{WDA: r.WDA, MJPEG: r.MJPEG}
		m.iosCtl.Set(r.DeviceID, ep) // make the screen usable immediately if procs are alive
		port := portOf(r.WDA)
		if port > 0 && probeWDA(port) == nil {
			m.log.Info("provision resume: screen alive", "device", r.DeviceID, "wda", r.WDA)
			continue
		}
		d, err := m.st.Device(r.DeviceID)
		if err != nil || d.UDID == "" || xctestrun == "" {
			m.log.Warn("provision resume: cannot respawn WDA", "device", r.DeviceID)
			continue
		}
		j := &Job{DeviceID: r.DeviceID, State: stateRunning, Step: "resuming", StartedAt: time.Now()}
		m.mu.Lock()
		m.jobs[r.DeviceID] = j
		m.mu.Unlock()
		go func(d model.Device, j *Job) {
			newEp, wp, err := m.startWDA(j, d.UDID, xctestrun)
			m.mu.Lock()
			if err != nil {
				j.State, j.Err = stateFailed, err.Error()
			} else {
				j.State, j.Step = stateDone, "ready"
			}
			m.mu.Unlock()
			if err == nil {
				m.iosCtl.Set(d.ID, newEp)
				_ = m.st.SetIOSScreen(store.IOSScreenRow{
					DeviceID: d.ID, WDA: newEp.WDA, MJPEG: newEp.MJPEG,
					WDARunPID: pidOf(wp.run), WDAPID: pidOf(wp.wda), MJPEGPID: pidOf(wp.mjpeg),
				})
			}
		}(d, j)
	}
}

func (m *Manager) wdaTeam() string {
	if v := os.Getenv("POLIGON_WDA_TEAM"); v != "" {
		return v
	}
	return m.cfg.IOSWDA.Team
}

func (m *Manager) usedPorts() map[int]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := map[int]bool{}
	for _, p := range m.procs {
		used[p.wdaPort] = true
		used[p.mjpegPort] = true
	}
	return used
}

// --- process helpers ---

func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func pidOf(c *exec.Cmd) int {
	if c == nil || c.Process == nil {
		return 0
	}
	return c.Process.Pid
}

// reap waits on a started command in the background so it doesn't become a
// zombie, without blocking the caller.
func reap(c *exec.Cmd) {
	if c == nil || c.Process == nil {
		return
	}
	go c.Wait()
}

func kill(c *exec.Cmd) error {
	if c == nil || c.Process == nil {
		return nil
	}
	return c.Process.Kill()
}

func killMatching(pattern string) error {
	return exec.Command("pkill", "-f", pattern).Run()
}

// spawnAndWaitForLine starts cmd and blocks until substr appears on its combined
// output or the timeout elapses. On timeout the process keeps running (WDA may
// just be slow) but an error is returned.
func (m *Manager) spawnAndWaitForLine(j *Job, cmd *exec.Cmd, substr string, timeout time.Duration) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	found := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "error:") || strings.Contains(line, "ERROR") {
				m.logf(j, "%s", strings.TrimSpace(line))
			}
			if strings.Contains(line, substr) {
				select {
				case found <- struct{}{}:
				default:
				}
			}
		}
	}()
	select {
	case <-found:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s waiting for %q", timeout, substr)
	}
}

// streamCmd runs cmd to completion, forwarding notable lines to the job log.
func (m *Manager) streamCmd(j *Job, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var tail []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		tail = append(tail, line)
		if len(tail) > 8 {
			tail = tail[1:]
		}
		if strings.Contains(line, "error:") || strings.HasPrefix(line, "** ") ||
			strings.Contains(line, "Signing") || strings.Contains(line, "CodeSign") {
			m.logf(j, "%s", line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%w\n%s", err, strings.Join(tail, "\n"))
	}
	return nil
}

// --- misc helpers ---

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func findXCTestRun(derivedData string) string {
	matches, _ := filepath.Glob(filepath.Join(derivedData, "Build", "Products", "WebDriverAgentRunner_*.xctestrun"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func freePort(from int, used map[int]bool) int {
	if from <= 0 {
		from = 18100
	}
	for p := from; p < from+500; p++ {
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		l.Close()
		return p
	}
	return from
}

func probeWDA(port int) error {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func portOf(hostPort string) int {
	_, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return 0
	}
	n := 0
	fmt.Sscan(p, &n)
	return n
}

func waitFor(cond func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("condition not met within %s", timeout)
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
