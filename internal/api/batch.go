package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
)

type batchCreateReq struct {
	Devices []string `json:"devices"`
}

func (s *Server) batchCreate(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var req batchCreateReq
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	batch, res, err := s.res.ReserveMany(req.Devices, u.Name)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": batch, "reservations": res})
}

func (s *Server) batchGet(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	ids, err := s.res.BatchDevices(r.PathValue("batch"), u.Name)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	out := make([]deviceView, 0, len(ids))
	for _, id := range ids {
		d, err := s.st.Device(id)
		if err != nil {
			continue
		}
		out = append(out, deviceView{Device: d})
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": r.PathValue("batch"), "devices": out})
}

func (s *Server) batchHeartbeat(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := s.res.HeartbeatBatch(r.PathValue("batch"), u.Name); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) batchRelease(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := s.res.ReleaseBatch(r.PathValue("batch"), u.Name, u.IsAdmin); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

// batchInstall uploads one artifact and installs it on every device in the
// batch, in parallel (bounded), re-signing an ipa once for all iOS targets.
func (s *Server) batchInstall(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	batch := r.PathValue("batch")

	ids, err := s.res.BatchDevices(batch, u.Name)
	if err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}

	if err := r.ParseMultipartForm(1 << 30); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	file, hdr, err := r.FormFile("artifact")
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	dir := filepath.Join(s.cfg.StorageDir, "uploads",
		time.Now().Format("20060102-150405")+"-batch-"+batch)
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

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()

	type deviceResult struct {
		Device  string `json:"device"`
		Status  string `json:"status"`
		Detail  string `json:"detail,omitempty"`
		Package string `json:"package,omitempty"`
	}
	results := make([]deviceResult, len(ids))

	const maxParallel = 4
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dev, err := s.st.Device(id)
			if err != nil {
				results[i] = deviceResult{Device: id, Status: "failed", Detail: err.Error()}
				return
			}
			_ = s.st.SetDeviceStatus(id, model.StatusBusy, time.Now())
			res, ierr := s.inst.Run(ctx, dev, artifactPath)
			if ierr != nil {
				results[i] = deviceResult{Device: id, Status: "failed", Detail: ierr.Error()}
			} else {
				results[i] = deviceResult{Device: id, Status: "ok", Package: res.Package, Detail: res.Output}
			}
			_ = s.st.SetDeviceStatus(id, model.StatusReserved, time.Now())
		}(i, id)
	}
	wg.Wait()

	ok := 0
	for _, rr := range results {
		if rr.Status == "ok" {
			ok++
		}
	}
	s.log.Info("batch install", "batch", batch, "user", u.Name,
		"artifact", hdr.Filename, "ok", ok, "total", len(ids))

	code := http.StatusOK
	if ok < len(ids) {
		code = http.StatusMultiStatus
	}
	writeJSON(w, code, map[string]any{"ok": ok, "total": len(ids), "results": results})
}

// readJSON is a tiny helper mirroring writeJSON.
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}
