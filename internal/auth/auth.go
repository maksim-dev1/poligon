// Package auth is minimal farm authentication: named users, each with a bearer
// token (stored hashed). Enough to attribute reservations and gate mutations.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/pancir/poligon/internal/model"
)

// ErrUnauthorized is returned when a token does not resolve to a user.
var ErrUnauthorized = errors.New("unauthorized")

type ctxKey struct{}

// Auth resolves tokens to users.
type Auth struct {
	db *sql.DB
}

// New builds an Auth over the shared DB handle.
func New(db *sql.DB) *Auth { return &Auth{db: db} }

// CreateUser adds a user and returns a freshly generated plaintext token
// (shown once).
func (a *Auth) CreateUser(name string, admin bool) (string, error) {
	tok := newToken()
	sum := hash(tok)
	_, err := a.db.Exec(
		`INSERT INTO users (name, token_hash, is_admin) VALUES (?, ?, ?)`,
		name, sum, boolToInt(admin))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// UserByToken looks up the user owning a token.
func (a *Auth) UserByToken(tok string) (model.User, error) {
	if tok == "" {
		return model.User{}, ErrUnauthorized
	}
	sum := hash(tok)
	rows, err := a.db.Query(`SELECT name, token_hash, is_admin, created_at FROM users`)
	if err != nil {
		return model.User{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var u model.User
		var admin int
		if err := rows.Scan(&u.Name, &u.TokenHash, &admin, &u.CreatedAt); err != nil {
			return model.User{}, err
		}
		if subtle.ConstantTimeCompare([]byte(u.TokenHash), []byte(sum)) == 1 {
			u.IsAdmin = admin == 1
			return u, nil
		}
	}
	return model.User{}, ErrUnauthorized
}

// Middleware requires a valid bearer token and stashes the user in the context.
// If devUser is non-empty, every request is treated as that user (local dev).
func (a *Auth) Middleware(devUser string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if devUser != "" {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{},
					model.User{Name: devUser, IsAdmin: true})))
				return
			}
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			u, err := a.UserByToken(tok)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
		})
	}
}

// UserFrom returns the authenticated user from a request context.
func UserFrom(ctx context.Context) (model.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(model.User)
	return u, ok
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
