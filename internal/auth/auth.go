// Package auth is poligon's authentication layer: email + password logins,
// server-side sessions, one-time "set your password" links, and the HTTP
// middleware that ties requests to a user. Legacy bearer tokens
// (users.token_hash) still resolve, for scripts predating the session flow.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

// ErrUnauthorized is a generic, non-revealing auth failure.
var ErrUnauthorized = errors.New("unauthorized")

// ErrRateLimited is returned when login attempts for a key are temporarily locked.
var ErrRateLimited = errors.New("too many attempts, try again later")

// ErrWeakPassword is returned when a chosen password fails the length policy.
var ErrWeakPassword = errors.New("password must be 10-72 characters")

// ErrBadEmail is returned when a registration email is not a plausible address.
var ErrBadEmail = errors.New("enter a valid email address")

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func validEmail(s string) bool { return len(s) <= 254 && emailRe.MatchString(s) }

const (
	sessionCookie = "poligon_session"
	csrfCookie    = "poligon_csrf"
	csrfHeader    = "X-Poligon-CSRF"

	minPassLen    = 10
	maxPassLen    = 72 // bcrypt truncates past 72 bytes
	bcryptCost    = 12
	maxLoginFails = 5
	lockWindow    = 15 * time.Minute
	setupTTL      = 72 * time.Hour
)

type ctxKey struct{}

// Options configures Auth.
type Options struct {
	SessionTTL  time.Duration
	SessionIdle time.Duration
	DevUser     string // POLIGON_DEV_USER
	DevAllow    bool   // honor DevUser (loopback listen or --dev)
	Log         *slog.Logger
}

// Auth resolves requests to users and issues sessions.
type Auth struct {
	st  *store.Store
	opt Options
	rl  *limiter
	log *slog.Logger
}

// New builds an Auth over the shared store.
func New(st *store.Store, o Options) *Auth {
	if o.SessionTTL == 0 {
		o.SessionTTL = 14 * 24 * time.Hour
	}
	if o.SessionIdle == 0 {
		o.SessionIdle = 24 * time.Hour
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if o.DevUser != "" && o.DevAllow {
		log.Warn("auth: dev bypass enabled — every request is treated as this user", "user", o.DevUser)
	} else if o.DevUser != "" {
		log.Warn("auth: POLIGON_DEV_USER set but ignored (listen is not loopback and --dev not passed)")
	}
	return &Auth{st: st, opt: o, rl: newLimiter(), log: log}
}

// --- provisioning ---

// CreateUser adds an account and returns a one-time "set your password" link
// token (raw, to embed in a URL). The user must set a password before login.
// Used by the host CLI; the dashboard uses Register instead.
func (a *Auth) CreateUser(name string) (setupToken string, err error) {
	name = normEmail(name)
	if name == "" {
		return "", errors.New("empty email")
	}
	if err := a.st.CreateUser(name); err != nil {
		return "", err
	}
	return a.newSetupToken(name)
}

// Register is open self-service signup: pick an email + password, get a session.
func (a *Auth) Register(w http.ResponseWriter, r *http.Request, email, password string) error {
	email = normEmail(email)
	if !validEmail(email) {
		return ErrBadEmail
	}
	if !validPassword(password) {
		return ErrWeakPassword
	}
	ipKey := "register|" + clientIP(r)
	if a.rl.blocked(ipKey) {
		return ErrRateLimited
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	if err := a.st.CreateUser(email); err != nil {
		if errors.Is(err, store.ErrUserExists) {
			a.rl.fail(ipKey)
			return store.ErrUserExists
		}
		return err
	}
	if err := a.st.SetPassword(email, string(h)); err != nil {
		return err
	}
	a.log.Info("auth: account registered", "user", email, "ip", clientIP(r))
	return a.issueSession(w, r, email)
}

// NewSetupToken issues a fresh set-password link for an existing user.
func (a *Auth) NewSetupToken(name string) (string, error) {
	name = normEmail(name)
	if _, err := a.st.User(name); err != nil {
		return "", err
	}
	return a.newSetupToken(name)
}

func (a *Auth) newSetupToken(name string) (string, error) {
	raw := randToken()
	if err := a.st.CreateEnrollToken(hash(raw), name, time.Now().Add(setupTTL)); err != nil {
		return "", err
	}
	return raw, nil
}

// SetupUser returns the account a valid, unspent set-password token belongs to.
func (a *Auth) SetupUser(rawToken string) (string, error) {
	user, err := a.st.EnrollTokenUser(hash(rawToken))
	if err != nil {
		return "", ErrUnauthorized
	}
	return user, nil
}

// SetPassword completes a set-password link: stores the hash, spends the token
// and signs the user in.
func (a *Auth) SetPassword(w http.ResponseWriter, r *http.Request, rawToken, password string) error {
	user, err := a.st.EnrollTokenUser(hash(rawToken))
	if err != nil {
		return ErrUnauthorized
	}
	if !validPassword(password) {
		return ErrWeakPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	if err := a.st.SetPassword(user, string(h)); err != nil {
		return err
	}
	if err := a.st.UseEnrollToken(hash(rawToken)); err != nil {
		return err
	}
	a.log.Info("auth: password set", "user", user)
	return a.issueSession(w, r, user)
}

// ChangePassword verifies the current password and swaps in a new one, then
// revokes every other session for the user.
func (a *Auth) ChangePassword(r *http.Request, current, next string) error {
	u, ok := UserFrom(r.Context())
	if !ok {
		return ErrUnauthorized
	}
	if !a.checkPassword(u.Name, current) {
		return ErrUnauthorized
	}
	if !validPassword(next) {
		return ErrWeakPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(next), bcryptCost)
	if err != nil {
		return err
	}
	if err := a.st.SetPassword(u.Name, string(h)); err != nil {
		return err
	}
	// keep the caller signed in, drop everyone else
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.st.RevokeUserSessionsExcept(u.Name, hash(c.Value))
	} else {
		_ = a.st.RevokeUserSessions(u.Name)
	}
	a.log.Info("auth: password changed", "user", u.Name)
	return nil
}

// --- login ---

// Login verifies email + password and issues a session cookie.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request, email, password string) error {
	email = normEmail(email)
	key := email + "|" + clientIP(r)
	if a.rl.blocked(key) {
		return ErrRateLimited
	}
	u, err := a.st.User(email)
	if err != nil || u.Disabled || !u.PassSet || !a.checkPassword(email, password) {
		a.rl.fail(key)
		return ErrUnauthorized
	}
	a.rl.ok(key)
	return a.issueSession(w, r, email)
}

// Logout revokes the caller's session and clears the cookies.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.st.RevokeSession(hash(c.Value))
	}
	a.clearCookies(w, a.secure(r))
}

func (a *Auth) checkPassword(user, password string) bool {
	h, err := a.st.UserPassHash(user)
	if err != nil || h == "" {
		// still run a hash to keep timing roughly constant for unknown users
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$0000000000000000000000000000000000000000000000000000a"), []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(h), []byte(password)) == nil
}

func validPassword(p string) bool {
	return len(p) >= minPassLen && len(p) <= maxPassLen
}

func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// --- sessions ---

func (a *Auth) issueSession(w http.ResponseWriter, r *http.Request, user string) error {
	raw := randToken()
	now := time.Now()
	sess := model.Session{
		User:      user,
		ExpiresAt: now.Add(a.opt.SessionIdle),
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	}
	if err := a.st.CreateSession(sess, hash(raw)); err != nil {
		return err
	}
	secure := a.secure(r)
	maxAge := int(a.opt.SessionTTL.Seconds())
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: randToken(), Path: "/", HttpOnly: false,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	})
	a.log.Info("auth: session issued", "user", user, "ip", clientIP(r))
	return nil
}

func (a *Auth) clearCookies(w http.ResponseWriter, secure bool) {
	for _, n := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: n, Value: "", Path: "/", HttpOnly: n == sessionCookie,
			Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
	}
}

// --- middleware ---

// Middleware authenticates a request and stashes the user in its context.
// Order: session cookie (with CSRF check on writes) -> legacy bearer -> dev user.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := a.fromSession(r); ok {
			if !safeMethod(r.Method) && !a.csrfOK(r) {
				http.Error(w, "bad CSRF token", http.StatusForbidden)
				return
			}
			a.serve(next, w, r, u)
			return
		}
		if u, ok := a.fromBearer(r); ok {
			a.serve(next, w, r, u)
			return
		}
		if a.opt.DevUser != "" && a.opt.DevAllow {
			a.serve(next, w, r, model.User{Name: a.opt.DevUser})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (a *Auth) serve(next http.Handler, w http.ResponseWriter, r *http.Request, u model.User) {
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
}

// Resolve authenticates a request without enforcing it — for handlers mounted
// outside Middleware (e.g. GET /auth/me). No CSRF check.
func (a *Auth) Resolve(r *http.Request) (model.User, bool) {
	if u, ok := a.fromSession(r); ok {
		return u, true
	}
	if u, ok := a.fromBearer(r); ok {
		return u, true
	}
	if a.opt.DevUser != "" && a.opt.DevAllow {
		return model.User{Name: a.opt.DevUser}, true
	}
	return model.User{}, false
}

func (a *Auth) fromSession(r *http.Request) (model.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return model.User{}, false
	}
	sess, u, err := a.st.SessionByToken(hash(c.Value))
	if err != nil {
		return model.User{}, false
	}
	if time.Since(sess.LastSeen) > time.Minute {
		exp := time.Now().Add(a.opt.SessionIdle)
		if hard := sess.CreatedAt.Add(a.opt.SessionTTL); exp.After(hard) {
			exp = hard
		}
		_ = a.st.TouchSession(sess.ID, time.Now(), exp)
	}
	return u, true
}

func (a *Auth) fromBearer(r *http.Request) (model.User, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return model.User{}, false
	}
	sum := hash(tok)

	// personal API tokens (the supported path for scripted / CI callers)
	if u, err := a.st.APITokenUser(sum); err == nil {
		return u, true
	}

	// legacy per-user bearer token (users.token_hash) — predates the session flow
	users, err := a.st.Users()
	if err != nil {
		return model.User{}, false
	}
	for _, u := range users {
		if u.TokenHash != "" && subtle.ConstantTimeCompare([]byte(u.TokenHash), []byte(sum)) == 1 {
			if u.Disabled {
				return model.User{}, false
			}
			return u, true
		}
	}
	return model.User{}, false
}

func (a *Auth) csrfOK(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	got := r.Header.Get(csrfHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

// UserFrom returns the authenticated user from a request context.
func UserFrom(ctx context.Context) (model.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(model.User)
	return u, ok
}

func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// secure reports whether responses for this request should set Secure cookies.
func (a *Auth) secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- login rate limiter ---

type limiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	fails int
	until time.Time
	seen  time.Time
}

func newLimiter() *limiter {
	l := &limiter{m: map[string]*bucket{}}
	go l.gc()
	return l
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	return b != nil && time.Now().Before(b.until)
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		b = &bucket{}
		l.m[key] = b
	}
	b.fails++
	b.seen = time.Now()
	if b.fails >= maxLoginFails {
		b.until = time.Now().Add(lockWindow)
		b.fails = 0
	}
}

func (l *limiter) ok(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (l *limiter) gc() {
	for range time.Tick(10 * time.Minute) {
		l.mu.Lock()
		for k, b := range l.m {
			if time.Since(b.seen) > lockWindow && time.Now().After(b.until) {
				delete(l.m, k)
			}
		}
		l.mu.Unlock()
	}
}
