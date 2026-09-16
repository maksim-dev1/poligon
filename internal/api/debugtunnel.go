package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/pancir/poligon/internal/model"
)

// The adb tunnel to farm devices is fully automatic now: it opens the moment
// a developer reserves an Android device (see reserveTunnel in api.go),
// self-heals on every reservation heartbeat, and closes once no Android
// device is held by anyone. There is nothing left to click per device — see
// deviceDebugTunnelInfo below for the one-time VS Code setup instead.

// deviceDebugTunnelInfo returns the farm's fixed adb-tunnel host/port, for the
// one-time VS Code setup (ANDROID_ADB_SERVER_ADDRESS/PORT in a shell profile
// or a dedicated VS Code profile). These values don't change per device or
// per session — this isn't a per-device endpoint, just a convenience so the
// setup instructions can show the real host instead of a placeholder.
func (s *Server) deviceDebugTunnelInfo(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ADBTunnelPort <= 0 {
		fail(w, http.StatusServiceUnavailable, errDebugTunnelDisabled)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host": host,
		"port": s.cfg.ADBTunnelPort,
	})
}

var errDebugTunnelDisabled = errors.New("debug tunnel disabled (adb_tunnel_port: 0)")

func portAddr(port int) string { return fmt.Sprintf(":%d", port) }

// reserveTunnel opens the adb tunnel when the caller reserves an Android
// device — no separate click needed, it just appears in VS Code once the
// device is held (see README's "VS Code / live debugging" setup).
func (s *Server) reserveTunnel(deviceID string) {
	dev, err := s.st.Device(deviceID)
	if err != nil || dev.Platform != model.Android || s.cfg.ADBTunnelPort <= 0 {
		return
	}
	_, _ = s.adbTunnel.Start(portAddr(s.cfg.ADBTunnelPort))
}

// releaseTunnel closes the tunnel once no Android device is held by anyone
// anymore — cheap to check, the farm is small.
func (s *Server) releaseTunnel() {
	devs, err := s.st.Devices()
	if err != nil {
		return
	}
	for _, d := range devs {
		if d.Platform == model.Android && d.Status != model.StatusFree {
			return
		}
	}
	s.adbTunnel.Stop()
}
