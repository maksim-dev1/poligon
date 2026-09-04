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
	if iosRunnerApp(dd) == "" {
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
		if iosRunnerApp(dd) == "" {
			return fmt.Errorf("build produced no WebDriverAgentRunner-Runner.app under %s", dd)
		}
	}

	// 3. run WDA (go-ios, not xcodebuild — Xcode 16's test session drops iOS 15
	//    devices) + forward ports
	ep, wp, err := m.startWDA(j, d.UDID, dd)
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

// startWDA installs WebDriverAgent (if needed), launches it via go-ios, and
// forwards its ports. go-ios talks to the device's own services instead of an
// xcodebuild test session, which Xcode 16 cannot keep alive against iOS 15/16.
// All processes are detached so the screen survives a poligon restart.
func (m *Manager) startWDA(j *Job, udid, dd string) (iosscreen.Endpoint, *wdaProc, error) {
	app := iosRunnerApp(dd)
	if app == "" {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("WebDriverAgent is not built (%s)", dd)
	}
	runnerID := bundleIDOf(app)
	if runnerID == "" {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("cannot read bundle id of %s", app)
	}
	xctestName := xctestConfigName(app)

	used := m.usedPorts()
	wdaPort := freePort(m.cfg.IOSWDA.WDAPortBase, used)
	used[wdaPort] = true
	mjpegPort := freePort(m.cfg.IOSWDA.MJPEGPortBase, used)

	m.step(j, "installing WebDriverAgent")
	if out, err := run(context.Background(), "ios", "install", "--path="+app, "--udid="+udid); err != nil {
		m.logf(j, "%s", oneLine(out))
		return iosscreen.Endpoint{}, nil, fmt.Errorf("ios install: %w", err)
	}

	m.step(j, "starting WebDriverAgent on the device")
	m.stopWDA("", udid) // clear any stale runner/forwards for this udid
	runCmd := exec.Command("ios", "runwda",
		"--bundleid="+runnerID,
		"--testrunnerbundleid="+runnerID,
		"--xctestconfig="+xctestName,
		"--udid="+udid,
		"--log-output=-")
	detach(runCmd)
	m.spawnLogging(j, runCmd) // starts it, forwards notable lines to the job log
	reap(runCmd)

	m.step(j, fmt.Sprintf("forwarding ports (wda:%d mjpeg:%d)", wdaPort, mjpegPort))
	wdaTun := exec.Command("ios", "forward", fmt.Sprint(wdaPort), "8100", "--udid="+udid)
	mjpegTun := exec.Command("ios", "forward", fmt.Sprint(mjpegPort), "9100", "--udid="+udid)
	detach(wdaTun)
	detach(mjpegTun)
	if err := wdaTun.Start(); err != nil {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("ios forward wda: %w", err)
	}
	if err := mjpegTun.Start(); err != nil {
		return iosscreen.Endpoint{}, nil, fmt.Errorf("ios forward mjpeg: %w", err)
	}
	reap(wdaTun)
	reap(mjpegTun)

	// readiness = WDA answers /status through the tunnel (more reliable than
	// grepping the runner's log for a specific line)
	if err := waitFor(func() bool { return probeWDA(wdaPort) == nil }, 120*time.Second); err != nil {
		_ = kill(runCmd)
		return iosscreen.Endpoint{}, nil, fmt.Errorf("WebDriverAgent did not become ready on :%d within 120s", wdaPort)
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
	dd := expandHome(m.cfg.IOSWDA.DerivedData)
	for _, r := range rows {
		ep := iosscreen.Endpoint{WDA: r.WDA, MJPEG: r.MJPEG}
		m.iosCtl.Set(r.DeviceID, ep) // make the screen usable immediately if procs are alive
		port := portOf(r.WDA)
		if port > 0 && probeWDA(port) == nil {
			m.log.Info("provision resume: screen alive", "device", r.DeviceID, "wda", r.WDA)
			continue
		}
		d, err := m.st.Device(r.DeviceID)
		if err != nil || d.UDID == "" || iosRunnerApp(dd) == "" {
			m.log.Warn("provision resume: cannot respawn WDA", "device", r.DeviceID)
			continue
		}
		j := &Job{DeviceID: r.DeviceID, State: stateRunning, Step: "resuming", StartedAt: time.Now()}
		m.mu.Lock()
		m.jobs[r.DeviceID] = j
		m.mu.Unlock()
		go func(d model.Device, j *Job) {
			newEp, wp, err := m.startWDA(j, d.UDID, dd)
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

// RestartScreen tears down and re-creates a device's live screen. Runs async as
// a job (keyed by device id) that the dashboard polls, same as adopt.
func (m *Manager) RestartScreen(deviceID string) (Job, error) {
	d, err := m.st.Device(deviceID)
	if err != nil {
		return Job{}, err
	}
	m.mu.Lock()
	if j, ok := m.jobs[deviceID]; ok && j.State == stateRunning {
		snap := j.copy()
		m.mu.Unlock()
		return snap, nil
	}
	j := &Job{DeviceID: deviceID, State: stateRunning, Step: "restarting screen", StartedAt: time.Now()}
	m.jobs[deviceID] = j
	m.mu.Unlock()

	go func() {
		var err error
		switch d.Platform {
		case model.IOS:
			err = m.restartIOS(d, j)
		case model.Android:
			err = m.restartAndroid(d, j)
		default:
			err = fmt.Errorf("unknown platform %q", d.Platform)
		}
		m.mu.Lock()
		if err != nil {
			j.State, j.Err = stateFailed, err.Error()
			m.log.Warn("restart screen failed", "device", deviceID, "err", err)
		} else {
			j.State, j.Step = stateDone, "ready"
			m.log.Info("screen restarted", "device", deviceID)
		}
		m.mu.Unlock()
	}()
	return j.copy(), nil
}

func (m *Manager) restartIOS(d model.Device, j *Job) error {
	if d.UDID == "" {
		return fmt.Errorf("device has no udid")
	}
	dd := expandHome(m.cfg.IOSWDA.DerivedData)
	if iosRunnerApp(dd) == "" {
		return fmt.Errorf("WebDriverAgent is not built yet — use \"connect to farm\" first")
	}
	m.step(j, "stopping WebDriverAgent")
	m.stopWDA(d.ID, d.UDID)
	time.Sleep(2 * time.Second)

	ep, wp, err := m.startWDA(j, d.UDID, dd)
	if err != nil {
		return err
	}
	m.iosCtl.Set(d.ID, ep)
	_ = m.st.SetIOSScreen(store.IOSScreenRow{
		DeviceID: d.ID, WDA: ep.WDA, MJPEG: ep.MJPEG,
		WDARunPID: pidOf(wp.run), WDAPID: pidOf(wp.wda), MJPEGPID: pidOf(wp.mjpeg),
	})
	return nil
}

func (m *Manager) restartAndroid(d model.Device, j *Job) error {
	if d.Serial == "" {
		return fmt.Errorf("device has no adb serial")
	}
	m.step(j, "restarting on-device screen server")
	bin := m.cfg.ADBPath
	if bin == "" {
		bin = "adb"
	}
	// ws-scrcpy leaves the scrcpy server holding tcp:8886 after a client drops;
	// killing it lets ws-scrcpy spawn a fresh one on the next connect.
	_ = exec.Command(bin, "-s", d.Serial, "shell", "pkill", "-9", "-f", "scrcpy").Run()
	return nil
}

// stopWDA kills the tracked WDA runner + iproxy tunnels for a device.
func (m *Manager) stopWDA(deviceID, udid string) {
	m.mu.Lock()
	wp := m.procs[deviceID]
	if wp == nil {
		wp = m.procs[udid]
	}
	delete(m.procs, deviceID)
	delete(m.procs, udid)
	m.mu.Unlock()
	if wp != nil {
		_ = kill(wp.run)
		_ = kill(wp.wda)
		_ = kill(wp.mjpeg)
	}
	_ = killMatching("ios runwda.*" + udid)
	_ = killMatching("ios forward.*" + udid)
	_ = killMatching("iproxy .*" + udid) // legacy, in case an old tunnel lingers
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

// spawnLogging starts cmd and forwards notable output lines to the job log in
// the background. It does not wait for readiness — the caller probes for that.
func (m *Manager) spawnLogging(j *Job, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if strings.Contains(line, wdaServerLine) ||
				strings.Contains(strings.ToLower(line), "error") ||
				strings.Contains(line, "level=fatal") {
				m.logf(j, "%s", line)
			}
		}
	}()
	return nil
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

// iosRunnerApp finds the built WebDriverAgentRunner-Runner.app.
func iosRunnerApp(derivedData string) string {
	for _, pat := range []string{
		filepath.Join(derivedData, "Build", "Products", "Debug-iphoneos", "*-Runner.app"),
		filepath.Join(derivedData, "Build", "Products", "*-iphoneos", "*-Runner.app"),
	} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// bundleIDOf reads CFBundleIdentifier from an .app's Info.plist.
func bundleIDOf(appPath string) string {
	out, err := exec.Command("plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-",
		filepath.Join(appPath, "Info.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// xctestConfigName is the .xctest bundle name inside the runner app's PlugIns.
func xctestConfigName(appPath string) string {
	m, _ := filepath.Glob(filepath.Join(appPath, "PlugIns", "*.xctest"))
	if len(m) > 0 {
		return filepath.Base(m[0])
	}
	return "WebDriverAgentRunner.xctest"
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
