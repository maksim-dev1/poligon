package auth

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pancir/poligon/internal/store"
)

const goodPass = "correct horse battery"

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, Options{
		SessionTTL:  time.Hour,
		SessionIdle: time.Hour,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// onboard creates a user and walks the set-password link.
func onboard(t *testing.T, a *Auth, email string) {
	t.Helper()
	tok, err := a.CreateUser(email)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := a.SetPassword(rec, req(), tok, goodPass); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("SetPassword issued no session cookie")
	}
}

func TestSetPasswordThenLogin(t *testing.T) {
	a := newTestAuth(t)
	onboard(t, a, "dev@pancir.io")

	rec := httptest.NewRecorder()
	if err := a.Login(rec, req(), "DEV@pancir.io", goodPass); err != nil {
		t.Fatalf("Login: %v", err)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	r := httptest.NewRequest("GET", "/api/devices", nil)
	r.AddCookie(session)
	u, ok := a.Resolve(r)
	if !ok || u.Name != "dev@pancir.io" {
		t.Fatalf("Resolve = %+v, %v", u, ok)
	}
}

func TestRegisterOpen(t *testing.T) {
	a := newTestAuth(t)
	rec := httptest.NewRecorder()
	if err := a.Register(rec, req(), "New@pancir.io", goodPass); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := a.Login(httptest.NewRecorder(), req(), "new@pancir.io", goodPass); err != nil {
		t.Fatalf("Login after Register: %v", err)
	}
	// same email again -> conflict
	if err := a.Register(httptest.NewRecorder(), req(), "new@pancir.io", goodPass); !errors.Is(err, store.ErrUserExists) {
		t.Fatalf("re-register: want ErrUserExists, got %v", err)
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	a := newTestAuth(t)
	if err := a.Register(httptest.NewRecorder(), req(), "not-an-email", goodPass); err != ErrBadEmail {
		t.Fatalf("want ErrBadEmail, got %v", err)
	}
	if err := a.Register(httptest.NewRecorder(), req(), "ok@pancir.io", "short"); err != ErrWeakPassword {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	a := newTestAuth(t)
	onboard(t, a, "u@pancir.io")
	if err := a.Login(httptest.NewRecorder(), req(), "u@pancir.io", "nope nope nope"); err == nil {
		t.Fatal("expected wrong password to fail")
	}
}

func TestWeakPasswordRejected(t *testing.T) {
	a := newTestAuth(t)
	tok, _ := a.CreateUser("weak@pancir.io")
	if err := a.SetPassword(httptest.NewRecorder(), req(), tok, "short"); err != ErrWeakPassword {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
}

func TestLoginRateLimited(t *testing.T) {
	a := newTestAuth(t)
	onboard(t, a, "rl@pancir.io")
	for range maxLoginFails {
		_ = a.Login(httptest.NewRecorder(), req(), "rl@pancir.io", "bad password here")
	}
	if err := a.Login(httptest.NewRecorder(), req(), "rl@pancir.io", "bad password here"); err != ErrRateLimited {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestDisabledUserCannotLogin(t *testing.T) {
	a := newTestAuth(t)
	onboard(t, a, "gone@pancir.io")
	if err := a.st.SetUserDisabled("gone@pancir.io", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Login(httptest.NewRecorder(), req(), "gone@pancir.io", goodPass); err == nil {
		t.Fatal("disabled user logged in")
	}
}

func TestSetupTokenSingleUse(t *testing.T) {
	a := newTestAuth(t)
	tok, err := a.CreateUser("once@pancir.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPassword(httptest.NewRecorder(), req(), tok, goodPass); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetupUser(tok); err == nil {
		t.Fatal("expected spent setup token to be rejected")
	}
}

func TestCSRFRequiredForCookieWrites(t *testing.T) {
	a := newTestAuth(t)
	onboard(t, a, "csrf@pancir.io")
	rec := httptest.NewRecorder()
	_ = a.Login(rec, req(), "csrf@pancir.io", goodPass)
	cookies := rec.Result().Cookies()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := a.Middleware(next)

	r1 := httptest.NewRequest("POST", "/api/devices/x/reserve", nil)
	for _, c := range cookies {
		r1.AddCookie(c)
	}
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, r1)
	if w1.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF: want 403, got %d", w1.Code)
	}

	var csrfVal string
	for _, c := range cookies {
		if c.Name == csrfCookie {
			csrfVal = c.Value
		}
	}
	r2 := httptest.NewRequest("POST", "/api/devices/x/reserve", nil)
	for _, c := range cookies {
		r2.AddCookie(c)
	}
	r2.Header.Set(csrfHeader, csrfVal)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("POST with CSRF: want 204, got %d", w2.Code)
	}
}

func req() *http.Request {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	return r
}
