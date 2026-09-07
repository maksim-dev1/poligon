package main

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/provision"
	"github.com/pancir/poligon/internal/store"
)

// depsWatchdog periodically logs the health of poligon's out-of-process
// dependencies (ws-scrcpy sidecar, go-ios tunnel, adb) and self-heals iOS
// screens whose WebDriverAgent has stopped answering.
func depsWatchdog(ctx context.Context, cfg config.Config, st *store.Store, prov *provision.Manager, log *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	type healState struct {
		fails       int
		healCount   int
		windowStart time.Time
	}
	heal := map[string]*healState{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		sidecar := reachable(cfg.LiveSidecar)
		tunnel := exec.Command("ios", "tunnel", "ls").Run() == nil
		adbN := adbDeviceCount(cfg.ADBPath)
		if !sidecar || !tunnel {
			log.Warn("deps", "ws_scrcpy", sidecar, "go_ios_tunnel", tunnel, "adb_devices", adbN)
		} else {
			log.Info("deps ok", "ws_scrcpy", sidecar, "go_ios_tunnel", tunnel, "adb_devices", adbN)
		}

		rows, err := st.IOSScreens()
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.WDA == "" {
				continue
			}
			hs := heal[r.DeviceID]
			if hs == nil {
				hs = &healState{windowStart: time.Now()}
				heal[r.DeviceID] = hs
			}
			if time.Since(hs.windowStart) > 10*time.Minute {
				hs.healCount, hs.windowStart = 0, time.Now()
			}

			if probeStatus("http://" + r.WDA + "/status") {
				hs.fails = 0
				continue
			}
			hs.fails++
			if hs.fails < 2 {
				continue
			}
			// don't fight a restart that's already running
			if j, ok := prov.Get(r.DeviceID); ok && j.State == "running" {
				continue
			}
			if hs.healCount >= 3 {
				log.Warn("watchdog: iOS screen still down, giving up for this window", "device", r.DeviceID)
				continue
			}
			hs.healCount++
			hs.fails = 0
			log.Warn("watchdog: iOS screen unresponsive — restarting", "device", r.DeviceID, "attempt", hs.healCount)
			_, _ = prov.RestartScreen(r.DeviceID)
		}
	}
}

func reachable(url string) bool {
	if url == "" {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Head(url)
	if err != nil {
		// some servers 405 HEAD — a GET that connects still counts
		resp, err = c.Get(url)
		if err != nil {
			return false
		}
	}
	resp.Body.Close()
	return true
}

func probeStatus(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func adbDeviceCount(adbPath string) int {
	if adbPath == "" {
		adbPath = "adb"
	}
	out, err := exec.Command(adbPath, "devices").Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, ln := range strings.Split(string(out), "\n")[1:] {
		if strings.HasSuffix(strings.TrimSpace(ln), "\tdevice") {
			n++
		}
	}
	return n
}
