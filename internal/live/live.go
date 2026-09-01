// Package live proxies the ws-scrcpy sidecar so the dashboard can show and
// control a device's screen. Access is gated: only the user holding a device's
// reservation may open its screen.
//
// ws-scrcpy runs locally (default 127.0.0.1:8000). poligon exposes it under
// /live/ with auth + reservation checks, including the WebSocket upgrade used
// for the H.264 stream and touch input.
package live

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/store"
)

// Proxy forwards /live/ traffic to the ws-scrcpy sidecar.
type Proxy struct {
	st    *store.Store
	res   *reserve.Manager
	log   *slog.Logger
	rp    *httputil.ReverseProxy
	ready bool
}

// New builds the proxy. target is the sidecar base URL, e.g.
// "http://127.0.0.1:8000". If target is empty the proxy reports not-configured.
func New(target string, st *store.Store, res *reserve.Manager, log *slog.Logger) *Proxy {
	p := &Proxy{st: st, res: res, log: log}
	if target == "" {
		return p
	}
	u, err := url.Parse(target)
	if err != nil {
		log.Error("live: bad sidecar url", "target", target, "err", err)
		return p
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Warn("live proxy error", "path", r.URL.Path, "err", err)
		http.Error(w, "live screen sidecar unavailable", http.StatusBadGateway)
	}
	p.rp = rp
	p.ready = true
	return p
}

// Handler returns the /live/ handler (mount with StripPrefix("/live")).
// devUser mirrors auth.Middleware's dev bypass.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.ready {
			http.Error(w, "live screen not configured", http.StatusNotImplemented)
			return
		}
		u, _ := auth.UserFrom(r.Context())

		// The stream/control endpoints carry the device serial or udid in the
		// query (ws-scrcpy uses ?udid=). Enforce the reservation on it.
		if dev := p.deviceForRequest(r); dev != "" {
			if !p.mayControl(dev, u) {
				http.Error(w, "reserve the device before opening its screen", http.StatusForbidden)
				return
			}
		}
		p.rp.ServeHTTP(w, r)
	})
}

// deviceForRequest resolves the poligon device id targeted by a live request,
// matching on the serial/udid ws-scrcpy passes as ?udid=.
func (p *Proxy) deviceForRequest(r *http.Request) string {
	q := r.URL.Query().Get("udid")
	if q == "" {
		// action=stream requests put it in the fragment on the client; the ws
		// URL still carries it as ?udid=. Anything without it is a static asset.
		return ""
	}
	pool, err := p.st.Devices()
	if err != nil {
		return ""
	}
	for _, d := range pool {
		if d.Serial == q || d.UDID == q {
			return d.ID
		}
	}
	return ""
}

func (p *Proxy) mayControl(deviceID string, u model.User) bool {
	res, ok, err := p.res.Holder(deviceID)
	if err != nil || !ok {
		return false
	}
	return res.User == u.Name || u.IsAdmin
}

// SidecarTargets lists the serial/udid values the sidecar should expose. Used to
// pass an allow-list to ws-scrcpy if we later run it in filtered mode.
func SidecarTargets(devs []model.Device) []string {
	out := make([]string, 0, len(devs))
	for _, d := range devs {
		if d.Serial != "" {
			out = append(out, d.Serial)
		}
		if d.UDID != "" {
			out = append(out, d.UDID)
		}
	}
	return out
}

// StreamPath builds the dashboard link that opens a device's screen.
func StreamPath(d model.Device) string {
	id := d.Serial
	if id == "" {
		id = d.UDID
	}
	// ws-scrcpy single-device deep link
	return "/live/#!action=stream&udid=" + url.QueryEscape(id) + "&player=mse"
}
