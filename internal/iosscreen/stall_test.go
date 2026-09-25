package iosscreen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A WDA mjpeg connection that stays open but goes silent must not hang the
// reader: it gives up after stallAfter so pump can reconnect.
func TestReadMJPEGGivesUpOnSilentStream(t *testing.T) {
	old := stallAfter
	stallAfter = 200 * time.Millisecond
	defer func() { stallAfter = old }()

	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=--BoundaryString")
		// one frame, then silence with the connection held open
		_, _ = w.Write([]byte("--BoundaryString\r\nContent-Type: image/jpeg\r\n\r\n\xff\xd8frame\xff\xd9\r\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := New(nil, Tuning{})
	s := newStream()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- c.readMJPEG(context.Background(), Endpoint{MJPEG: strings.TrimPrefix(srv.URL, "http://")}, s)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no data") {
			t.Fatalf("err = %v", err)
		}
		if s.frames() != 1 {
			t.Fatalf("frames = %d, want the one sent before the stall", s.frames())
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader hung on a silent stream")
	}
}
