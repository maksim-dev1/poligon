package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/auth"
)

// Staged uploads let a caller that cannot send multipart in the same request —
// an MCP agent, whose tool calls carry JSON only — push a build or a flow
// workspace first and then refer to it by id:
//
//	curl -F file=@app.apk -H "Authorization: Bearer plgn_…" https://farm/api/uploads
//	→ {"upload_id":"u_3f9a…","name":"app.apk","size":48213456}
//
// Each upload lives in its own dir with an .owner file, so only its uploader
// can use it; dirs older than uploadTTL are swept on the next upload.

const uploadTTL = 48 * time.Hour

var uploadIDRe = regexp.MustCompile(`^u_[0-9a-f]{16}$`)

func (s *Server) uploadsDir() string { return filepath.Join(s.cfg.StorageDir, "uploads", "staged") }

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	fh := r.MultipartForm.File["file"]
	if len(fh) != 1 {
		fail(w, http.StatusBadRequest, errors.New("attach exactly one file as field \"file\""))
		return
	}
	s.sweepUploads()

	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := "u_" + hex.EncodeToString(b)
	dir := filepath.Join(s.uploadsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ".owner"), []byte(u.Name), 0o644); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	path, err := saveUpload(fh[0], dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id": id,
		"name":      filepath.Base(path),
		"size":      fh[0].Size,
		"expires":   time.Now().Add(uploadTTL).UTC().Format(time.RFC3339),
	})
}

// stagedUpload resolves an upload id to its file, for its owner only.
func (s *Server) stagedUpload(user, id string) (string, error) {
	if !uploadIDRe.MatchString(id) {
		return "", errors.New("bad upload_id (want u_ + 16 hex, from POST /api/uploads)")
	}
	dir := filepath.Join(s.uploadsDir(), id)
	owner, err := os.ReadFile(filepath.Join(dir, ".owner"))
	if err != nil || string(owner) != user {
		return "", errors.New("upload " + id + " not found (expired after 48h, or someone else's)")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", errors.New("upload " + id + " is empty")
}

func (s *Server) sweepUploads() {
	ents, err := os.ReadDir(s.uploadsDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > uploadTTL {
			_ = os.RemoveAll(filepath.Join(s.uploadsDir(), e.Name()))
		}
	}
}
