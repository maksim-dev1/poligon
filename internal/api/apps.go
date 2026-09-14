package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pancir/poligon/internal/adb"
)

func (s *Server) deviceAppsList(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	withSystem := r.URL.Query().Get("all") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	pkgs, err := s.capt.ListPackages(ctx, dev, withSystem)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	if pkgs == nil {
		pkgs = []adb.Package{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": pkgs})
}

func decodePackage(r *http.Request) (string, error) {
	var body struct {
		Package string `json:"package"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Package == "" {
		return "", errors.New("missing package")
	}
	return body.Package, nil
}

func (s *Server) deviceAppForceStop(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	pkg, err := decodePackage(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.capt.ForceStopApp(ctx, dev, pkg); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deviceAppClearData(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	pkg, err := decodePackage(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.capt.ClearAppData(ctx, dev, pkg); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deviceAppUninstall(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	pkg, err := decodePackage(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out, err := s.capt.UninstallApp(ctx, dev, pkg)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": out})
}

func (s *Server) deviceAppLaunch(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	pkg, err := decodePackage(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.capt.LaunchApp(ctx, dev, pkg); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deviceUIDump streams the current screen's view hierarchy as XML — the
// browser renders it as a collapsible tree natively when opened directly.
func (s *Server) deviceUIDump(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	xml, err := s.capt.UIDump(ctx, dev)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(xml))
}

func (s *Server) deviceOpenSettings(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	var body struct {
		Screen string `json:"screen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.capt.OpenSettings(ctx, dev, body.Screen); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
