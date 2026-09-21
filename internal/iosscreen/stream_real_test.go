package iosscreen

import (
	"bytes"
	"image/jpeg"
	"mime"
	"mime/multipart"
	"os"
	"testing"
)

// realCapture is a raw body recorded from a farm iPhone's WebDriverAgent mjpeg
// server (POLIGON_WDA_CAPTURE=<file> to point at one). WDA declares
// `boundary=--BoundaryString`, i.e. the value already carries the two dashes a
// MIME delimiter starts with, so a spec-following multipart reader looks for
// `----BoundaryString` and never sees a single part. That shipped once; this
// test exists so it cannot ship twice.
func realCapture(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("POLIGON_WDA_CAPTURE")
	if path == "" {
		t.Skip("set POLIGON_WDA_CAPTURE to a raw WDA mjpeg body to run this")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	return b
}

func TestScanJPEGsReadsRealWDAStream(t *testing.T) {
	body := realCapture(t)

	s := newStream()
	done := make(chan struct{})
	var frames [][]byte
	go func() {
		defer close(done)
		_ = scanJPEGs(bytes.NewReader(body), s)
	}()
	<-done

	// collect whatever the scanner published by walking the stream's last frame
	// plus the count it reports
	if got := s.frames(); got < 10 {
		t.Fatalf("scanned only %d frames out of a %d byte capture", got, len(body))
	}
	last, _, err := (&Sub{s: s}).Latest()
	if err != nil {
		t.Fatalf("no frame available: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(last)); err != nil {
		t.Fatalf("scanned frame is not a decodable JPEG: %v", err)
	}
	frames = append(frames, last)
	t.Logf("scanned %d frames, last one decodes as %d bytes", s.frames(), len(frames[0]))
}

// TestMultipartReaderFailsOnWDABoundary pins down *why* the boundary is not
// trusted, so nobody "simplifies" readMJPEG back to mime/multipart.
func TestMultipartReaderFailsOnWDABoundary(t *testing.T) {
	body := realCapture(t)

	const contentType = "multipart/x-mixed-replace; boundary=--BoundaryString"
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	if _, err := mr.NextPart(); err == nil {
		t.Fatal("multipart reader unexpectedly found a part — the boundary quirk is gone, " +
			"readMJPEG could use mime/multipart again")
	}
}
