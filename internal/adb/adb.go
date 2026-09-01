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
	return sp, nil
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
