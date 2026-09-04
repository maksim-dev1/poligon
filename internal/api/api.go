// Package api is poligon's HTTP surface: the JSON API and the embedded dashboard.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/live"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/provision"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/store"
	"github.com/pancir/poligon/internal/webui"
)

// Server holds the API dependencies.
type Server struct {
	cfg  config.Config
	st   *store.Store
	res  *reserve.Manager
	inst *install.Installer
	live *live.Proxy
	ios  *iosscreen.Controller
	prov *provision.Manager
	log  *slog.Logger
	web  http.FileSystem
}

// New builds the API server.
func New(cfg config.Config, st *store.Store, res *reserve.Manager, inst *install.Installer, lp *live.Proxy, ios *iosscreen.Controller, prov *provision.Manager, web http.FileSystem, log *slog.Logger) *Server {
	return &Server{cfg: cfg, st: st, res: res, inst: inst, live: lp, ios: ios, prov: prov, web: web, log: log}
}

// Handler returns the root http.Handler with auth applied to /api.
func (s *Server) Handler(a *auth.Auth, devUser string) http.Handler {
	mux := http.NewServeMux()

	api := http.NewServeMux()
	api.HandleFunc("GET /devices", s.listDevices)
	api.HandleFunc("GET /devices/{id}", s.getDevice)
	api.HandleFunc("POST /devices/{id}/reserve", s.reserve)
	api.HandleFunc("POST /devices/{id}/release", s.release)
	api.HandleFunc("POST /devices/{id}/heartbeat", s.heartbeat)
	api.HandleFunc("POST /devices/{id}/install", s.install)
	api.HandleFunc("GET /devices/{id}/screen", s.screenLink)
	api.HandleFunc("POST /devices/{id}/screen/restart", s.restartScreen)
	api.HandleFunc("POST /devices/{id}/adopt", s.adoptDevice)
	api.HandleFunc("GET /devices/{id}/adopt", s.adoptStatus)
	api.HandleFunc("GET /devices/{id}/job", s.adoptStatus)
	api.HandleFunc("POST /session", s.session)

	// multi-device batches: reserve a set, install once to all, one grid of screens
	api.HandleFunc("POST /batches", s.batchCreate)
	api.HandleFunc("GET /batches/{batch}", s.batchGet)
	api.HandleFunc("POST /batches/{batch}/install", s.batchInstall)
	api.HandleFunc("POST /batches/{batch}/heartbeat", s.batchHeartbeat)
	api.HandleFunc("POST /batches/{batch}/release", s.batchRelease)

	// iOS live screen (WebDriverAgent-backed): player page + MJPEG + input.
	ios := http.NewServeMux()
	ios.HandleFunc("GET /ios/{id}", s.iosScreenPage)
	ios.HandleFunc("GET /ios/{id}/size", s.iosSize)
	ios.HandleFunc("GET /ios/{id}/mjpeg", s.iosMJPEG)
	ios.HandleFunc("GET /ios/{id}/frame", s.iosFrame)
	ios.HandleFunc("POST /ios/{id}/input", s.iosInput)
	ios.HandleFunc("POST /ios/{id}/restart", s.iosRestart)
	ios.HandleFunc("GET /ios/{id}/job", s.iosJob)
	ios.HandleFunc("GET /grid", s.screenGrid)

	mux.Handle("/api/", http.StripPrefix("/api", a.Middleware(devUser)(api)))
	mux.Handle("/live/grid", http.StripPrefix("/live", a.Middleware(devUser)(ios)))
	mux.Handle("/live/ios/", http.StripPrefix("/live", a.Middleware(devUser)(ios)))
	mux.Handle("/live/", http.StripPrefix("/live", a.Middleware(devUser)(s.live.Handler())))
	mux.Handle("/", http.FileServer(s.web))
	return logging(s.log, mux)
}

// --- device views ---

type deviceView struct {
	model.Device
	Reservation *model.Reservation `json:"reservation,omitempty"`
	Job         *provision.Job     `json:"job,omitempty"` // active/last adopt job, candidates only
}

func (s *Server) fillView(d model.Device) deviceView {
	dv := deviceView{Device: d}
	if res, ok, _ := s.res.Holder(d.ID); ok {
		dv.Reservation = &res
	}
	if !d.Adopted {
		if j, ok := s.prov.Get(d.ID); ok {
			dv.Job = &j
		}
	}
	return dv
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	pool, err := s.st.Devices()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]deviceView, 0, len(pool))
	for _, d := range pool {
		out = append(out, s.fillView(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, s.fillView(d))
}

// adoptDevice starts (or re-attaches to) a candidate device's preparation job.
func (s *Server) adoptDevice(w http.ResponseWriter, r *http.Request) {
	job, err := s.prov.Start(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// adoptStatus returns the current preparation job for a device.
func (s *Server) adoptStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.prov.Get(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("no preparation job for this device"))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// --- reservations ---

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	res, err := s.res.Reserve(r.PathValue("id"), u.Name)
	switch {
	case errors.Is(err, reserve.ErrTaken):
		fail(w, http.StatusConflict, err)
	case errors.Is(err, reserve.ErrUnavailable):
		fail(w, http.StatusConflict, err)
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	err := s.res.Release(r.PathValue("id"), u.Name, u.IsAdmin)
	if errors.Is(err, reserve.ErrNotHolder) {
		fail(w, http.StatusForbidden, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := s.res.Heartbeat(r.PathValue("id"), u.Name); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// session sets a cookie from the caller's bearer token so that browser
// navigations to /live/ (iframe, WebSocket) authenticate without a header.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get("Authorization")
	tok = tok[len("Bearer "):]
	http.SetCookie(w, &http.Cookie{
		Name: "poligon_token", Value: tok, Path: "/",
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- iOS live screen ---

// iosHolder checks the caller holds the device's reservation and iOS screen is
// configured; it writes the error response and returns false on failure.
func (s *Server) iosHolder(w http.ResponseWriter, r *http.Request) (string, bool) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if !s.ios.Configured(id) {
		fail(w, http.StatusNotImplemented, errors.New("iOS screen not configured for this device"))
		return "", false
	}
	if res, ok, _ := s.res.Holder(id); !ok || (res.User != u.Name && !u.IsAdmin) {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return "", false
	}
	return id, true
}

func (s *Server) screenGrid(w http.ResponseWriter, r *http.Request) {
	page, err := webui.File("grid.html")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) iosScreenPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.iosHolder(w, r); !ok {
		return
	}
	page, err := webui.File("ios-screen.html")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) iosSize(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	wpx, hpx, err := s.ios.Size(id)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"w": wpx, "h": hpx})
}

func (s *Server) iosMJPEG(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	h, err := s.ios.MJPEGHandler(id)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	// strip our path so the upstream sees "/"
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/"
	r2.URL.RawPath = "/"
	h.ServeHTTP(w, r2)
}

// iosFrame returns one JPEG from the device — the polling fallback for browsers
// that will not render a multipart <img> (Safari).
func (s *Server) iosFrame(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	jpg, err := s.ios.Frame(id)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(jpg)
}

func (s *Server) iosInput(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	var in iosscreen.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ios.Do(id, in); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// iosRestart tears down and re-creates the device's WebDriverAgent screen.
func (s *Server) iosRestart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	job, err := s.prov.RestartScreen(id)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// iosJob returns the current provision/restart job for the device.
func (s *Server) iosJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	job, jok := s.prov.Get(id)
	if !jok {
		writeJSON(w, http.StatusOK, map[string]string{"state": "none"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// restartScreen (bearer-auth, used from the grid) restarts a device's live
// screen: WebDriverAgent for iOS, the scrcpy server for Android.
func (s *Server) restartScreen(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if res, ok, _ := s.res.Holder(id); !ok || (res.User != u.Name && !u.IsAdmin) {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return
	}
	job, err := s.prov.RestartScreen(id)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// screenLink returns the live-screen URL for a device the caller may control.
func (s *Server) screenLink(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	d, err := s.st.Device(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if res, ok, _ := s.res.Holder(id); !ok || (res.User != u.Name && !u.IsAdmin) {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return
	}
	if d.Platform == model.IOS {
		if !s.ios.Configured(id) {
			fail(w, http.StatusNotImplemented, errors.New("iOS screen not configured for this device"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": "/live/ios/" + id})
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	writeJSON(w, http.StatusOK, map[string]string{"url": live.StreamPath(d, r.Host, secure)})
}

// --- install ---

func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")

	dev, err := s.st.Device(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	// only the holder may install
	if res, ok, _ := s.res.Holder(id); !ok || (res.User != u.Name && !u.IsAdmin) {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return
	}

	if err := r.ParseMultipartForm(512 << 20); err != nil { // 512 MiB
		fail(w, http.StatusBadRequest, err)
		return
	}
	file, hdr, err := r.FormFile("artifact")
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	dir := filepath.Join(s.cfg.StorageDir, "uploads", time.Now().Format("20060102-150405")+"-"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	artifactPath := filepath.Join(dir, filepath.Base(hdr.Filename))
	dst, err := os.Create(artifactPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		fail(w, http.StatusInternalServerError, err)
		return
	}
	dst.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	_ = s.st.SetDeviceStatus(id, model.StatusBusy, time.Now())
	result, ierr := s.inst.Run(ctx, dev, artifactPath)

	status, detail := "ok", result.Output
	code := http.StatusOK
	if ierr != nil {
		status, detail = "failed", ierr.Error()
		code = http.StatusBadGateway
	}
	s.log.Info("install", "device", id, "user", u.Name, "artifact", hdr.Filename, "status", status)

	writeJSON(w, code, map[string]any{
		"status":  status,
		"detail":  detail,
		"package": result.Package,
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "code", sw.code, "dur", time.Since(start).String())
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

// Hijack lets the WebSocket reverse proxy (/live/) take over the connection.
func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter is not a Hijacker")
	}
	return h.Hijack()
}

// Flush supports streaming responses.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
