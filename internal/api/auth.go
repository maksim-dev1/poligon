package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/store"
	"github.com/pancir/poligon/internal/webui"
)

// authLogin verifies email + password and sets the session cookie.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	err := s.auth.Login(w, r, in.Email, in.Password)
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		fail(w, http.StatusTooManyRequests, err)
	case err != nil:
		fail(w, http.StatusUnauthorized, errors.New("invalid email or password"))
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// authLogout revokes the caller's session.
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// authMe reports the current user, or 401.
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.auth.Resolve(r)
	if !ok {
		fail(w, http.StatusUnauthorized, errors.New("not signed in"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": u.Name})
}

// authRegister is open self-service signup.
func (s *Server) authRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	err := s.auth.Register(w, r, in.Email, in.Password)
	switch {
	case errors.Is(err, auth.ErrBadEmail), errors.Is(err, auth.ErrWeakPassword):
		fail(w, http.StatusBadRequest, err)
	case errors.Is(err, store.ErrUserExists):
		fail(w, http.StatusConflict, errors.New("an account with this email already exists — sign in instead"))
	case errors.Is(err, auth.ErrRateLimited):
		fail(w, http.StatusTooManyRequests, err)
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// authChangePassword swaps the caller's password (requires the current one).
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	err := s.auth.ChangePassword(r, in.Current, in.New)
	switch {
	case errors.Is(err, auth.ErrWeakPassword):
		fail(w, http.StatusBadRequest, err)
	case err != nil:
		fail(w, http.StatusForbidden, errors.New("current password is wrong"))
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// setupPage serves the "choose your password" page.
func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	page, err := webui.File("setup.html")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// setupCheck validates a set-password token and returns the target email.
func (s *Server) setupCheck(w http.ResponseWriter, r *http.Request) {
	email, err := s.auth.SetupUser(r.URL.Query().Get("t"))
	if err != nil {
		fail(w, http.StatusUnauthorized, errors.New("invalid or expired link"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

// setupSubmit stores the chosen password, spends the token and signs the user in.
func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		T        string `json:"t"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	err := s.auth.SetPassword(w, r, in.T, in.Password)
	switch {
	case errors.Is(err, auth.ErrWeakPassword):
		fail(w, http.StatusBadRequest, err)
	case err != nil:
		fail(w, http.StatusUnauthorized, errors.New("invalid or expired link"))
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
