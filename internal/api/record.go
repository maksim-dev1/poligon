package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/pancir/poligon/internal/adb"
)

func (s *Server) deviceRecordStart(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	if err := s.capt.StartRecording(dev); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deviceRecordStop(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.capt.StopRecording(ctx, dev); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	// screenrecord needs a moment after SIGINT to flush and close the mp4
	// container before it's safe to pull.
	time.Sleep(700 * time.Millisecond)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deviceRecordDownload(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	tmp, err := os.CreateTemp("", "poligon-rec-*.mp4")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.capt.PullFile(ctx, dev, adb.RecordPath, tmpPath); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", dev.ID+"-"+time.Now().Format("20060102-150405")+".mp4"))
	_, _ = io.Copy(w, f)
}
