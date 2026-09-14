// Package adb is a thin wrapper around the `adb` CLI for the Android side of the
// farm: listing devices, reading properties, installing apps.
package adb

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// ADB runs adb commands.
type ADB struct {
	bin string
}

// New returns an ADB wrapper using the given binary (e.g. "adb").
func New(bin string) *ADB {
	if bin == "" {
		bin = "adb"
	}
	return &ADB{bin: bin}
}

func (a *ADB) run(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("adb %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (a *ADB) shell(ctx context.Context, serial string, cmd ...string) (string, error) {
	return a.run(ctx, append([]string{"-s", serial, "shell"}, cmd...)...)
}

// Serials returns every serial adb currently sees, mapped to its state
// ("device", "unauthorized", "offline", "no permissions", ...).
func (a *ADB) Serials(ctx context.Context) (map[string]string, error) {
	out, err := a.run(ctx, "devices")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 2 {
			m[f[0]] = f[1]
		}
	}
	return m, nil
}

// OnlineSerials returns the serials adb reports as "device" (ready to use).
func (a *ADB) OnlineSerials(ctx context.Context) (map[string]bool, error) {
	all, err := a.Serials(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for s, state := range all {
		if state == "device" {
			set[s] = true
		}
	}
	return set, nil
}

// Reconnect attempts a soft recovery of a flaky device.
func (a *ADB) Reconnect(ctx context.Context, serial string) error {
	_, err := a.run(ctx, "-s", serial, "reconnect")
	return err
}

// Specs reads hardware/OS characteristics for one device.
func (a *ADB) Specs(ctx context.Context, serial string) (model.Specs, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	sp := model.Specs{Battery: -1}
	getprop := func(k string) string {
		v, _ := a.shell(ctx, serial, "getprop", k)
		return strings.TrimSpace(v)
	}
	sp.Model = getprop("ro.product.model")
	sp.Manufacturer = getprop("ro.product.manufacturer")
	sp.SoC = firstNonEmpty(getprop("ro.soc.model"), getprop("ro.board.platform"))
	sp.OSVersion = getprop("ro.build.version.release")
	sp.APILevel = getprop("ro.build.version.sdk")
	abi := getprop("ro.product.cpu.abi")
	if abi != "" {
		sp.SoC = strings.TrimSpace(sp.SoC + " (" + abi + ")")
	}

	if mem, err := a.shell(ctx, serial, "cat", "/proc/meminfo"); err == nil {
		for _, l := range strings.Split(mem, "\n") {
			if strings.HasPrefix(l, "MemTotal:") {
				sp.RAM = kbToHuman(strings.Fields(l))
			}
		}
	}
	if df, err := a.shell(ctx, serial, "df", "-h", "/data"); err == nil {
		lines := strings.Split(strings.TrimSpace(df), "\n")
		if len(lines) >= 2 {
			f := strings.Fields(lines[len(lines)-1])
			if len(f) >= 2 {
				sp.Storage = f[1]
			}
		}
	}
	if size, err := a.shell(ctx, serial, "wm", "size"); err == nil {
		sp.ScreenSize = lastField(size, ":")
	}
	if den, err := a.shell(ctx, serial, "wm", "density"); err == nil {
		sp.ScreenDensity = lastField(den, ":")
	}
	if bat, err := a.shell(ctx, serial, "dumpsys", "battery"); err == nil {
		for _, l := range strings.Split(bat, "\n") {
			l = strings.TrimSpace(l)
			if v, ok := strings.CutPrefix(l, "level: "); ok {
				fmt.Sscan(v, &sp.Battery)
			}
			if v, ok := strings.CutPrefix(l, "temperature: "); ok {
				sp.BatteryTempC = tenthsToC(v)
			}
		}
	}

	// probe simulated-input support: KEYCODE_UNKNOWN does nothing on screen but
	// still goes through injectInputEvent, so a SecurityException here means the
	// OS blocks synthetic taps (MIUI "USB debugging (Security settings)" off).
	out, ierr := a.shell(ctx, serial, "input", "keyevent", "0")
	probe := out
	if ierr != nil {
		probe += " " + ierr.Error()
	}
	if strings.Contains(probe, "SecurityException") || strings.Contains(probe, "INJECT_EVENTS") {
		sp.InputInjection = "blocked"
	} else if ierr == nil {
		sp.InputInjection = "ok"
	}
	return sp, nil
}

// Keyevent sends a hardware/navigation key press (see `adb shell input
// keyevent --help` / Android's KeyEvent constants) — e.g. 3=HOME, 4=BACK,
// 26=POWER, 24/25=VOLUME_UP/DOWN, 187=APP_SWITCH.
func (a *ADB) Keyevent(ctx context.Context, serial string, code int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "input", "keyevent", fmt.Sprint(code))
	return err
}

// shQuote single-quotes s for safe inclusion in a remote shell command line.
// `adb shell a b c` joins its args with spaces and runs the result through
// /system/bin/sh on the device, so any argument built from user input (a
// path, say) must be quoted here — the local exec.Command args are never
// shell-interpreted, but the remote side's are.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// FileEntry describes one entry in a device directory listing.
type FileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

// ListDir lists one directory on the device (parsed from `ls -laL`, since
// Android's toybox/busybox ls doesn't support machine-readable output).
// -L follows symlinks so a linked directory (e.g. legacy vendor
// /sdcard/sdcard compat symlinks) reports as a "d" entry instead of "l" —
// otherwise it would look like a file and a click would try to download it.
func (a *ADB) ListDir(ctx context.Context, serial, path string) ([]FileEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := a.shell(ctx, serial, "ls", "-laL", shQuote(path))
	if err != nil {
		return nil, err
	}
	var entries []FileEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		name := strings.Join(f[7:], " ")
		if name == "." || name == ".." {
			continue
		}
		if strings.HasPrefix(f[0], "l") { // symlink: "name -> target"
			if i := strings.Index(name, " -> "); i >= 0 {
				name = name[:i]
			}
		}
		var size int64
		fmt.Sscan(f[4], &size)
		entries = append(entries, FileEntry{Name: name, Dir: strings.HasPrefix(f[0], "d"), Size: size})
	}
	return entries, nil
}

// Pull copies a device file to a local path.
func (a *ADB) Pull(ctx context.Context, serial, remote, local string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err := a.run(ctx, "-s", serial, "pull", remote, local)
	return err
}

// Push copies a local file to a device path.
func (a *ADB) Push(ctx context.Context, serial, local, remote string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err := a.run(ctx, "-s", serial, "push", local, remote)
	return err
}

// Remove deletes a file or directory (recursively) on the device.
func (a *ADB) Remove(ctx context.Context, serial, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "rm", "-rf", "--", shQuote(path))
	return err
}

// Rename moves/renames a device path.
func (a *ADB) Rename(ctx context.Context, serial, from, to string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "mv", "--", shQuote(from), shQuote(to))
	return err
}

// ShellCommand builds (but does not start) an interactive `adb shell` for a
// caller to wire stdio to — e.g. a WebSocket terminal bridge. `-tt` forces a
// PTY on the device side regardless of the local process's own stdio, so the
// remote shell behaves interactively (prompts, job control, colors).
func (a *ADB) ShellCommand(ctx context.Context, serial string) *exec.Cmd {
	return exec.CommandContext(ctx, a.bin, "-s", serial, "shell", "-tt")
}

// LogcatCommand builds (but does not start) a continuous `adb logcat` for a
// caller to stream — e.g. a WebSocket live-tail. Unlike LogcatDump this never
// exits on its own; the caller cancels ctx to stop it.
func (a *ADB) LogcatCommand(ctx context.Context, serial string) *exec.Cmd {
	return exec.CommandContext(ctx, a.bin, "-s", serial, "logcat", "-v", "threadtime")
}

// Package describes one installed app.
type Package struct {
	Name   string `json:"name"`
	System bool   `json:"system"`
}

// ListPackages lists installed apps. Third-party (user-installed) ones are
// always included; withSystem also includes platform/OEM packages.
func (a *ADB) ListPackages(ctx context.Context, serial string, withSystem bool) ([]Package, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	parse := func(out string) map[string]bool {
		set := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			if name, ok := strings.CutPrefix(strings.TrimSpace(line), "package:"); ok && name != "" {
				set[name] = true
			}
		}
		return set
	}
	user, err := a.shell(ctx, serial, "pm", "list", "packages", "-3")
	if err != nil {
		return nil, err
	}
	userSet := parse(user)
	pkgs := make([]Package, 0, len(userSet))
	for name := range userSet {
		pkgs = append(pkgs, Package{Name: name})
	}
	if withSystem {
		all, err := a.shell(ctx, serial, "pm", "list", "packages")
		if err != nil {
			return nil, err
		}
		for name := range parse(all) {
			if !userSet[name] {
				pkgs = append(pkgs, Package{Name: name, System: true})
			}
		}
	}
	return pkgs, nil
}

// ForceStop kills every process of a package.
func (a *ADB) ForceStop(ctx context.Context, serial, pkg string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "am", "force-stop", shQuote(pkg))
	return err
}

// ClearData wipes a package's data and cache (like Settings > Storage > Clear
// data) without uninstalling it.
func (a *ADB) ClearData(ctx context.Context, serial, pkg string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "pm", "clear", shQuote(pkg))
	return err
}

// Uninstall removes a package.
func (a *ADB) Uninstall(ctx context.Context, serial, pkg string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return a.run(ctx, "-s", serial, "uninstall", pkg)
}

// RecordPath is the fixed on-device path a recording is written to and
// pulled from — one active recording per device at a time, same as the
// farm's "one run at a time" model elsewhere.
const RecordPath = "/sdcard/poligon-record.mp4"

// StartScreenRecord starts `screenrecord` in the background. It is
// fire-and-forget: the local adb process is intentionally not tied to the
// caller's context (a request context dies with the HTTP response, which
// would kill the recording instantly) and is reaped quietly whenever it
// exits — either the caller's StopScreenRecord signals it, or Android's own
// ~3 min hard cap on `screenrecord` ends it first.
func (a *ADB) StartScreenRecord(serial string) error {
	cmd := exec.Command(a.bin, "-s", serial, "shell", "screenrecord", "--time-limit", "180", RecordPath)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// StopScreenRecord sends SIGINT to the remote `screenrecord` process so it
// finalizes the mp4 container properly (killing the connection instead
// leaves a file most players can't open).
func (a *ADB) StopScreenRecord(ctx context.Context, serial string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "pkill", "-INT", "screenrecord")
	return err
}

// UIDump captures the current screen's view hierarchy as XML
// (`uiautomator dump`) — element ids/text/bounds, handy for locating a
// selector for a test or bug report.
func (a *ADB) UIDump(ctx context.Context, serial string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	const tmp = "/sdcard/poligon-ui-dump.xml"
	// uiautomator often exits 0 even when it fails, printing the reason to
	// stdout instead (e.g. "ERROR: could not get idle state." when the
	// screen is off/locked, or a UI is mid-animation) — catch that here
	// rather than serving a silent empty dump.
	dumpOut, err := a.shell(ctx, serial, "uiautomator", "dump", tmp)
	if err != nil {
		return "", err
	}
	if !strings.Contains(dumpOut, "dumped to") {
		return "", fmt.Errorf("uiautomator dump: %s", strings.TrimSpace(dumpOut))
	}
	out, err := a.shell(ctx, serial, "cat", tmp)
	_, _ = a.shell(ctx, serial, "rm", "-f", tmp)
	if err == nil && strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("uiautomator wrote an empty dump")
	}
	return out, err
}

// settingsScreens whitelists the `android.settings.*` intent actions the
// webui can jump to — free-text intent actions aren't accepted since they'd
// otherwise let a caller launch an arbitrary activity.
var settingsScreens = map[string]string{
	"wifi":      "android.settings.WIFI_SETTINGS",
	"bluetooth": "android.settings.BLUETOOTH_SETTINGS",
	"display":   "android.settings.DISPLAY_SETTINGS",
	"battery":   "android.settings.BATTERY_SAVER_SETTINGS",
	"datetime":  "android.settings.DATE_SETTINGS",
	"location":  "android.settings.LOCATION_SOURCE_SETTINGS",
	"apps":      "android.settings.APPLICATION_SETTINGS",
	"developer": "android.settings.APPLICATION_DEVELOPMENT_SETTINGS",
}

// SettingsScreens lists the screen keys OpenSettings accepts.
func SettingsScreens() map[string]string { return settingsScreens }

// OpenSettings jumps the device to one whitelisted system settings screen.
func (a *ADB) OpenSettings(ctx context.Context, serial, screen string) error {
	action, ok := settingsScreens[screen]
	if !ok {
		return fmt.Errorf("unknown settings screen %q", screen)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "am", "start", "-a", action)
	return err
}

// Install pushes an APK to a device. reinstall keeps app data; grant gives all
// runtime permissions up front.
func (a *ADB) Install(ctx context.Context, serial, apkPath string, reinstall, grant bool) (string, error) {
	args := []string{"-s", serial, "install"}
	if reinstall {
		args = append(args, "-r")
	}
	if grant {
		args = append(args, "-g")
	}
	args = append(args, apkPath)
	return a.run(ctx, args...)
}

// InstallMultiple installs an app split set (from an .aab / .apks).
func (a *ADB) InstallMultiple(ctx context.Context, serial string, apks []string, reinstall bool) (string, error) {
	args := []string{"-s", serial, "install-multiple"}
	if reinstall {
		args = append(args, "-r")
	}
	args = append(args, apks...)
	return a.run(ctx, args...)
}

// Launch starts the app's launcher activity.
func (a *ADB) Launch(ctx context.Context, serial, pkg string) error {
	_, err := a.shell(ctx, serial, "monkey", "-p", pkg, "-c", "android.intent.category.LAUNCHER", "1")
	return err
}

// Instrument runs an androidx.test instrumentation (`am instrument -w`) and
// returns its raw output for the caller to interpret: a clean run ends
// "OK (N tests)"; a failure prints "FAILURES!!!" plus a stack trace. No
// timeout shorter than the caller's ctx — an integration_test suite can
// legitimately run for minutes.
func (a *ADB) Instrument(ctx context.Context, serial, testPackage, runnerClass string) (string, error) {
	return a.shell(ctx, serial, "am", "instrument", "-w", testPackage+"/"+runnerClass)
}

// Screenshot grabs the current framebuffer as PNG bytes (raw, no adb newline
// translation — `exec-out` keeps the stream binary-clean).
func (a *ADB) Screenshot(ctx context.Context, serial string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, a.bin, "-s", serial, "exec-out", "screencap", "-p")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("adb screencap: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// LogcatDump returns the current logcat buffer (non-blocking, -d) and then
// clears it, so each call yields only what happened since the last one.
func (a *ADB) LogcatDump(ctx context.Context, serial string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := a.run(ctx, "-s", serial, "logcat", "-d", "-v", "threadtime")
	if err != nil {
		return out, err
	}
	_, _ = a.run(ctx, "-s", serial, "logcat", "-c")
	return out, nil
}

// LogcatClear empties the logcat buffer — call before a test run so the dump
// afterwards is scoped to the run.
func (a *ADB) LogcatClear(ctx context.Context, serial string) error {
	_, err := a.run(ctx, "-s", serial, "logcat", "-c")
	return err
}

// Running reports whether a process for pkg is currently alive on the device.
func (a *ADB) Running(ctx context.Context, serial, pkg string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := a.shell(ctx, serial, "pidof", pkg)
	if err != nil {
		// pidof exits 1 when nothing matches — that is "not running", not an error
		if strings.TrimSpace(out) == "" {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func lastField(s, sep string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, sep); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func kbToHuman(fields []string) string {
	if len(fields) < 2 {
		return ""
	}
	var kb float64
	fmt.Sscan(fields[1], &kb)
	return fmt.Sprintf("%.1f GB", kb/1024/1024)
}

func tenthsToC(v string) string {
	var t float64
	fmt.Sscan(strings.TrimSpace(v), &t)
	return fmt.Sprintf("%.1f", t/10)
}
