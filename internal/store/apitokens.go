package store

import (
	"database/sql"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// APIToken is a personal access token record (never holds the raw secret).
type APIToken struct {
	Hash       string     `json:"-"`
	HashPrefix string     `json:"hash_prefix"` // first 8 chars, for display/revoke
	User       string     `json:"user"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// CreateAPIToken stores the hash of a new token for a user.
func (s *Store) CreateAPIToken(hash, user, name string) error {
	_, err := s.db.Exec(
		`INSERT INTO api_tokens (token_hash, user_name, name) VALUES (?, ?, ?)`,
		hash, user, name)
	return err
}

// APITokenUser resolves a token hash to its (enabled) owner and bumps
// last_used_at. Returns ErrNoUser when the token is unknown or the owner is
// disabled.
func (s *Store) APITokenUser(hash string) (model.User, error) {
	var (
		u        model.User
		disabled int
		passSet  int
	)
	err := s.db.QueryRow(
		`SELECT u.name, u.token_hash, u.disabled, u.pass_set, u.created_at
		   FROM api_tokens t JOIN users u ON u.name = t.user_name
		  WHERE t.token_hash = ?`, hash).
		Scan(&u.Name, &u.TokenHash, &disabled, &passSet, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return model.User{}, ErrNoUser
	}
	if err != nil {
		return model.User{}, err
	}
	if disabled != 0 {
		return model.User{}, ErrNoUser
	}
	u.Disabled = false
	u.PassSet = passSet != 0
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`, time.Now(), hash)
	return u, nil
}

// APITokens lists a user's tokens (no secrets).
func (s *Store) APITokens(user string) ([]APIToken, error) {
	rows, err := s.db.Query(
		`SELECT token_hash, user_name, name, created_at, last_used_at
		   FROM api_tokens WHERE user_name = ? ORDER BY created_at DESC`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var (
			t    APIToken
			last sql.NullTime
		)
		if err := rows.Scan(&t.Hash, &t.User, &t.Name, &t.CreatedAt, &last); err != nil {
			return nil, err
		}
		if len(t.Hash) >= 8 {
			t.HashPrefix = t.Hash[:8]
		}
		if last.Valid {
			t.LastUsedAt = &last.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPITokenByPrefix removes a user's token whose hash starts with prefix.
// Returns the number of rows deleted (0 or 1).
func (s *Store) DeleteAPITokenByPrefix(user, prefix string) (int, error) {
	res, err := s.db.Exec(
		`DELETE FROM api_tokens WHERE user_name = ? AND token_hash LIKE ? || '%'`,
		user, prefix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
