package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/store"
)

// Personal API tokens, self-service from the dashboard (the host CLI
// `poligon token …` manages the same table). A token is what a script, CI job
// or coding agent (MCP) authenticates with; its raw value is shown once.

var hashPrefixRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	toks, err := s.st.APITokens(u.Name)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if toks == nil {
		toks = []store.APIToken{}
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(in.Name)
	if len(name) > 60 {
		fail(w, http.StatusBadRequest, errors.New("name: 60 characters max"))
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	raw := "plgn_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	h := hex.EncodeToString(sum[:])
	if err := s.st.CreateAPIToken(h, u.Name, name); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("api token created", "user", u.Name, "name", name, "prefix", h[:8])
	writeJSON(w, http.StatusOK, map[string]string{"token": raw, "hash_prefix": h[:8], "name": name})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	prefix := r.PathValue("prefix")
	if !hashPrefixRe.MatchString(prefix) {
		fail(w, http.StatusBadRequest, errors.New("bad token id"))
		return
	}
	n, err := s.st.DeleteAPITokenByPrefix(u.Name, prefix)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if n == 0 {
		fail(w, http.StatusNotFound, errors.New("no such token"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
