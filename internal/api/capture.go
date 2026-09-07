package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
)

// heldDevice resolves the device and confirms the caller currently holds it
// (directly or as part of a batch). It writes the error response itself.
func (s *Server) heldDevice(w http.ResponseWriter, r *http.Request) (model.Device, bool) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	dev, err := s.st.Device(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return model.Device{}, false
	}
	if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return model.Device{}, false
	}
	return dev, true
}

// deviceScreenshot streams a single still of the device screen.
func (s *Server) deviceScreenshot(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	img, mime, err := s.capt.Screenshot(ctx, dev)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	ext := "png"
	if mime == "image/jpeg" {
		ext = "jpg"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", dev.ID+"-"+time.Now().Format("20060102-150405")+"."+ext))
	_, _ = w.Write(img)
}

// deviceLogcat returns the device log buffer since the previous call.
func (s *Server) deviceLogcat(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	text, err := s.capt.Logcat(ctx, dev)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", dev.ID+"-"+time.Now().Format("20060102-150405")+".log"))
	_, _ = w.Write([]byte(text))
}
