package api

import (
	"errors"
	"net"
	"net/http"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
)

// The adb tunnel to farm devices is fully automatic and per-user: it opens
// (once, for life) the moment a developer first reserves an Android device,
// self-heals on every reservation heartbeat, and its allow-list shrinks back
// to nothing once they hold no Android device — see syncUserTunnel below.
// Each user gets their own port (internal/adbfilter), filtered to just the
// devices they currently hold, so one developer's `adb devices` never shows
// another's reservation. See deviceDebugTunnelInfo for the one-time VS Code
// setup that hands out that personal port.

// deviceDebugTunnelInfo returns the farm host + the caller's personal
// adb-tunnel port, for the one-time VS Code setup (ANDROID_ADB_SERVER_ADDRESS
// /PORT in a shell profile or a dedicated VS Code profile). The port is
// assigned once per user and never changes, so this is a one-time lookup.
func (s *Server) deviceDebugTunnelInfo(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ADBTunnelPort <= 0 {
		fail(w, http.StatusServiceUnavailable, errDebugTunnelDisabled)
		return
	}
	u, _ := auth.UserFrom(r.Context())
	port, err := s.st.TunnelPort(u.Name, s.cfg.ADBTunnelPortRangeStart, s.cfg.ADBTunnelPortRangeEnd)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.adbFilter.Ensure(u.Name, port); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host": host,
		"port": port,
	})
}

var errDebugTunnelDisabled = errors.New("debug tunnel disabled (adb_tunnel_port: 0)")

// syncUserTunnel (re)computes which ADB serials user is currently allowed to
// see through their personal tunnel — every Android device they hold an
// active reservation for, nothing else — and opens their listener on first
// use. Call after any reserve/release/heartbeat/batch operation for that
// user, and periodically (see main.go's reap loop) to catch silent expiry.
func (s *Server) syncUserTunnel(user string) {
	if s.cfg.ADBTunnelPort <= 0 {
		return
	}
	devs, err := s.st.Devices()
	if err != nil {
		return
	}
	var serials []string
	for _, d := range devs {
		if d.Platform != model.Android || d.Serial == "" {
			continue
		}
		if res, ok, _ := s.res.Holder(d.ID); ok && res.User == user {
			serials = append(serials, d.Serial)
		}
	}
	port, err := s.st.TunnelPort(user, s.cfg.ADBTunnelPortRangeStart, s.cfg.ADBTunnelPortRangeEnd)
	if err != nil {
		return
	}
	if err := s.adbFilter.Ensure(user, port); err != nil {
		return
	}
	s.adbFilter.SetAllowed(user, serials)
}

// SyncADBTunnels refreshes every user's tunnel allow-list — called
// periodically so a reservation that expires without an explicit release
// (see reserve.Manager.ReapExpired) still drops out of that user's `adb
// devices` promptly instead of only on their next heartbeat.
func (s *Server) SyncADBTunnels() {
	for _, user := range s.adbFilter.Users() {
		s.syncUserTunnel(user)
	}
}
