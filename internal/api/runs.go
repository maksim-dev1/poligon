package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/runner"
)

// createRun starts a test run. Multipart form:
//
//	type          run type (default "install_smoke")
//	device        repeated device ids (the devices to run on)
//	watch_seconds smoke settle window (optional)
//	artifact      one build file per platform (.apk/.aab for Android, .ipa for iOS)
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())

	if err := r.ParseMultipartForm(1 << 30); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	typ := r.FormValue("type")
	if typ == "" {
		typ = "install_smoke"
	}
	if !runner.Types[typ] {
		fail(w, http.StatusBadRequest, errors.New("unknown run type "+typ))
		return
	}
	devices := r.Form["device"]
	if len(devices) == 0 {
		fail(w, http.StatusBadRequest, errors.New("no devices"))
		return
	}

	headers := r.MultipartForm.File["artifact"]
	if len(headers) == 0 {
		fail(w, http.StatusBadRequest, errors.New("no artifact uploaded"))
		return
	}
	dir := filepath.Join(s.cfg.StorageDir, "uploads",
		time.Now().Format("20060102-150405")+"-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	spec := model.RunSpec{Artifacts: map[model.Platform]string{}}
	for _, hdr := range headers {
		plat := platformForExt(hdr.Filename)
		if plat == "" {
			fail(w, http.StatusBadRequest, errors.New("unsupported artifact "+hdr.Filename))
			return
		}
		if _, dup := spec.Artifacts[plat]; dup {
			fail(w, http.StatusBadRequest, errors.New("more than one "+string(plat)+" artifact"))
			return
		}
		p, err := saveUpload(hdr, dir)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		spec.Artifacts[plat] = p
	}
	if v := r.FormValue("watch_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			spec.WatchSeconds = n
		}
	}

	run, err := s.run.Submit(u.Name, typ, spec, devices, "ui")
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	runs, err := s.st.Runs(limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.Run(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	owner, err := s.st.RunOwner(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if owner != u.Name {
		fail(w, http.StatusForbidden, errors.New("not your run"))
		return
	}
	if !s.run.Cancel(id) {
		fail(w, http.StatusConflict, errors.New("run is not active"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceling"})
}

// runArtifact serves one file written by a run, e.g.
// GET /api/runs/{id}/artifacts/{device}/logcat.txt
func (s *Server) runArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	device := r.PathValue("device")
	name := r.PathValue("name")
	if strings.ContainsAny(id+device+name, "/\\") || strings.Contains(name, "..") {
		fail(w, http.StatusBadRequest, errors.New("bad path"))
		return
	}
	if _, err := s.st.Run(id); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	root := filepath.Join(s.cfg.StorageDir, "runs")
	path := filepath.Join(root, id, device, name)
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		fail(w, http.StatusBadRequest, errors.New("bad path"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}
