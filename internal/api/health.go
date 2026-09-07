package api

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// healthz is an unauthenticated snapshot of poligon and its out-of-process
// dependencies, for the dashboard's status dot and for ops tooling.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"poligon": "ok"}

	out["ws_scrcpy"] = headOK(s.cfg.LiveSidecar)

	tctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out["go_ios_tunnel"] = exec.CommandContext(tctx, "ios", "tunnel", "ls").Run() == nil

	rows, _ := s.st.IOSScreens()
	seen := map[string]bool{}
	total, ready := 0, 0
	for _, row := range rows {
		// an offline device's stale row may still point at another device's WDA —
		// don't count it, and don't credit the same endpoint twice
		if d, err := s.st.Device(row.DeviceID); err == nil && d.Status == "offline" {
			continue
		}
		total++
		if row.WDA != "" && !seen[row.WDA] && getOK("http://"+row.WDA+"/status") {
			seen[row.WDA] = true
			ready++
		}
	}
	out["ios_screens_total"] = total
	out["ios_screens_ready"] = ready
	out["sessions"] = s.st.SessionCount()
	out["adb_devices"] = adbCount(s.cfg.ADBPath)

	ok := out["ws_scrcpy"] == true && out["go_ios_tunnel"] == true && ready == len(rows)
	out["ok"] = ok

	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, out)
}

func headOK(url string) bool {
	if url == "" {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Head(url)
	if err != nil {
		resp, err = c.Get(url)
		if err != nil {
			return false
		}
	}
	resp.Body.Close()
	return true
}

func getOK(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func adbCount(adbPath string) int {
	if adbPath == "" {
		adbPath = "adb"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, adbPath, "devices").Output()
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
