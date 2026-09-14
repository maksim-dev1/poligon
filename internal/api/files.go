package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// filesRoot is where the browser lands by default — the shared, world-visible
// storage (/sdcard == /storage/emulated/0) most farm work touches. The browser
// itself isn't fenced to it: `adb shell` already runs as the unprivileged
// "shell" user, so the OS's own file permissions are the real boundary —
// most of /data is unreadable to it regardless of what path we ask for.
const filesRoot = "/sdcard"

// safeDevicePath only rejects a non-absolute path; Clean collapses any "..".
func safeDevicePath(p string) (string, error) {
	if p == "" {
		p = filesRoot
	}
	clean := path.Clean(p)
	if !strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("path must be absolute")
	}
	return clean, nil
}

// isTopLevel reports whether p is "/" or a direct child of it ("/sdcard",
// "/data", …) — a mutation guard against wiping an entire top-level tree by
// accident; everything below that stays fair game.
func isTopLevel(p string) bool {
	return p == "/" || strings.Count(strings.Trim(p, "/"), "/") == 0
}

func (s *Server) deviceFilesList(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	p, err := safeDevicePath(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	entries, err := s.capt.ListFiles(ctx, dev, p)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "entries": entries})
}

func (s *Server) deviceFilesDownload(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	p, err := safeDevicePath(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	tmp, err := os.CreateTemp("", "poligon-dl-*")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := s.capt.PullFile(ctx, dev, p, tmpPath); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(p)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

func (s *Server) deviceFilesUpload(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(256 << 20); err != nil { // 256 MiB
		fail(w, http.StatusBadRequest, err)
		return
	}
	dir, err := safeDevicePath(r.FormValue("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "poligon-up-*")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	tmpPath := tmp.Name()
	_, err = io.Copy(tmp, file)
	tmp.Close()
	defer os.Remove(tmpPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	remote := path.Join(dir, path.Base(hdr.Filename))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := s.capt.PushFile(ctx, dev, tmpPath, remote); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": remote})
}

func (s *Server) deviceFilesDelete(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	p, err := safeDevicePath(body.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if isTopLevel(p) {
		fail(w, http.StatusBadRequest, errors.New("refusing to delete a top-level directory"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.capt.RemovePath(ctx, dev, p); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deviceFilesRename(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	p, err := safeDevicePath(body.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if isTopLevel(p) {
		fail(w, http.StatusBadRequest, errors.New("refusing to rename a top-level directory"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || strings.ContainsAny(name, "/\x00") {
		fail(w, http.StatusBadRequest, errors.New("bad name"))
		return
	}
	to := path.Join(path.Dir(p), name)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.capt.RenamePath(ctx, dev, p, to); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": to})
}
