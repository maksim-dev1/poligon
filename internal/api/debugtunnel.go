package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/pancir/poligon/internal/model"
)

// deviceDebugTunnelStart opens (or keepalives) the shared adb network tunnel
// so the caller's local `flutter run -d <serial>` / VS Code can attach to
// this device directly. See internal/adbtunnel for what "shared" means here.
func (s *Server) deviceDebugTunnelStart(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	if dev.Platform != model.Android {
		fail(w, http.StatusBadRequest, errors.New("VS Code debug tunnel supports Android only for now"))
		return
	}
	if s.cfg.ADBTunnelPort <= 0 {
		fail(w, http.StatusServiceUnavailable, errors.New("debug tunnel disabled (adb_tunnel_port: 0)"))
		return
	}
	port, err := s.adbTunnel.Start(fmt.Sprintf(":%d", s.cfg.ADBTunnelPort))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host":   host,
		"port":   port,
		"serial": dev.Serial,
	})
}

// deviceDebugTunnelPing keeps an already-open tunnel alive; call every ~30s
// while the developer still wants it open. Self-healing: it re-opens the
// tunnel (Start is idempotent — a no-op if already listening) rather than
// just touching a timestamp, so a poligon restart mid-debug-session (a
// routine deploy, say) doesn't leave the tab silently pinging a tunnel that
// no longer exists.
func (s *Server) deviceDebugTunnelPing(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.heldDevice(w, r); !ok {
		return
	}
	if s.cfg.ADBTunnelPort <= 0 {
		fail(w, http.StatusServiceUnavailable, errors.New("debug tunnel disabled (adb_tunnel_port: 0)"))
		return
	}
	if _, err := s.adbTunnel.Start(fmt.Sprintf(":%d", s.cfg.ADBTunnelPort)); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deviceDebugTunnelStop closes the tunnel immediately.
func (s *Server) deviceDebugTunnelStop(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.heldDevice(w, r); !ok {
		return
	}
	s.adbTunnel.Stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
