package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// ErrNoUser is returned when a lookup finds no matching user.
var ErrNoUser = errors.New("user not found")

// ErrNoSession is returned when a session token does not resolve to a live session.
var ErrNoSession = errors.New("session not found")

// --- users ---

// ErrUserExists is returned when registering an email that already has an account.
var ErrUserExists = errors.New("account already exists")

// CreateUser inserts a farm account with no password yet (setup pending).
// token_hash is written explicitly as ” so this works on pre-existing databases
// whose users.token_hash column has NOT NULL without a default.
func (s *Store) CreateUser(name string) error {
	_, err := s.db.Exec(`INSERT INTO users (name, token_hash) VALUES (?, '')`, name)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrUserExists
	}
	return err
}

// User returns a single account by login id.
func (s *Store) User(name string) (model.User, error) {
	var u model.User
	var disabled, passSet int
	err := s.db.QueryRow(
		`SELECT name, token_hash, disabled, pass_set, created_at
		   FROM users WHERE name = ?`, name,
	).Scan(&u.Name, &u.TokenHash, &disabled, &passSet, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, ErrNoUser
	}
	if err != nil {
		return model.User{}, err
	}
	u.Disabled, u.PassSet = disabled == 1, passSet == 1
	return u, nil
}

// Users lists every account, ordered by login id.
func (s *Store) Users() ([]model.User, error) {
	rows, err := s.db.Query(
		`SELECT name, token_hash, disabled, pass_set, created_at
		   FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		var u model.User
		var disabled, passSet int
		if err := rows.Scan(&u.Name, &u.TokenHash, &disabled, &passSet, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Disabled, u.PassSet = disabled == 1, passSet == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserPassHash returns the stored bcrypt hash for a user ("" if unset).
func (s *Store) UserPassHash(name string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT pass_hash FROM users WHERE name = ?`, name).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoUser
	}
	return h, err
}

// SetPassword stores a bcrypt hash and marks the account ready to log in.
func (s *Store) SetPassword(name, hash string) error {
	_, err := s.db.Exec(
		`UPDATE users SET pass_hash = ?, pass_set = 1 WHERE name = ?`, hash, name)
	return err
}

// ClearPassword drops a user's password so a fresh setup link can be issued.
func (s *Store) ClearPassword(name string) error {
	_, err := s.db.Exec(
		`UPDATE users SET pass_hash = '', pass_set = 0 WHERE name = ?`, name)
	return err
}

// SetUserDisabled toggles an account and, when disabling, revokes its sessions.
func (s *Store) SetUserDisabled(name string, disabled bool) error {
	if _, err := s.db.Exec(`UPDATE users SET disabled = ? WHERE name = ?`, b2i(disabled), name); err != nil {
		return err
	}
	if disabled {
		return s.RevokeUserSessions(name)
	}
	return nil
}

// --- sessions ---

// CreateSession records a new login session.
func (s *Store) CreateSession(sess model.Session, tokenHash string) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (user_name, token_hash, expires_at, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?)`,
		sess.User, tokenHash, sess.ExpiresAt, sess.IP, sess.UserAgent)
	return err
}

// SessionByToken resolves a live (unexpired, unrevoked) session and its user.
func (s *Store) SessionByToken(tokenHash string) (model.Session, model.User, error) {
	var sess model.Session
	var revoked int
	err := s.db.QueryRow(
		`SELECT id, user_name, created_at, last_seen, expires_at, revoked
		   FROM sessions WHERE token_hash = ?`, tokenHash,
	).Scan(&sess.ID, &sess.User, &sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Session{}, model.User{}, ErrNoSession
	}
	if err != nil {
		return model.Session{}, model.User{}, err
	}
	sess.Revoked = revoked == 1
	if sess.Revoked || time.Now().After(sess.ExpiresAt) {
		return model.Session{}, model.User{}, ErrNoSession
	}
	u, err := s.User(sess.User)
	if err != nil {
		return model.Session{}, model.User{}, err
	}
	if u.Disabled {
		return model.Session{}, model.User{}, ErrNoSession
	}
	return sess, u, nil
}

// TouchSession slides a session's idle window forward.
func (s *Store) TouchSession(id int64, lastSeen, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE id = ?`,
		lastSeen, expiresAt, id)
	return err
}

// RevokeSession marks one session dead (logout).
func (s *Store) RevokeSession(tokenHash string) error {
	_, err := s.db.Exec(`UPDATE sessions SET revoked = 1 WHERE token_hash = ?`, tokenHash)
	return err
}

// RevokeUserSessions marks every session for a user dead (logout everywhere).
func (s *Store) RevokeUserSessions(name string) error {
	_, err := s.db.Exec(`UPDATE sessions SET revoked = 1 WHERE user_name = ?`, name)
	return err
}

// RevokeUserSessionsExcept revokes all of a user's sessions but the one whose
// token hashes to keep (used after a self-service password change).
func (s *Store) RevokeUserSessionsExcept(name, keepTokenHash string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET revoked = 1 WHERE user_name = ? AND token_hash != ?`,
		name, keepTokenHash)
	return err
}

// SessionCount returns the number of live (unrevoked, unexpired) sessions.
func (s *Store) SessionCount() int {
	var n int
	_ = s.db.QueryRow(
		`SELECT count(*) FROM sessions WHERE revoked = 0 AND expires_at > ?`,
		time.Now()).Scan(&n)
	return n
}

// PurgeExpiredSessions deletes revoked or long-expired session rows.
func (s *Store) PurgeExpiredSessions() error {
	_, err := s.db.Exec(
		`DELETE FROM sessions WHERE revoked = 1 OR expires_at < ?`,
		time.Now().Add(-24*time.Hour))
	return err
}

// --- enrollment tokens ---

// CreateEnrollToken stores a one-time enrollment link token for a user.
func (s *Store) CreateEnrollToken(tokenHash, user string, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO enroll_tokens (token_hash, user_name, expires_at) VALUES (?, ?, ?)`,
		tokenHash, user, expiresAt)
	return err
}

// EnrollTokenUser returns the user an unused, unexpired enrollment token belongs to.
func (s *Store) EnrollTokenUser(tokenHash string) (string, error) {
	var user string
	var used sql.NullTime
	var exp time.Time
	err := s.db.QueryRow(
		`SELECT user_name, expires_at, used_at FROM enroll_tokens WHERE token_hash = ?`, tokenHash,
	).Scan(&user, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSession
	}
	if err != nil {
		return "", err
	}
	if used.Valid || time.Now().After(exp) {
		return "", ErrNoSession
	}
	return user, nil
}

// UseEnrollToken marks an enrollment token spent.
func (s *Store) UseEnrollToken(tokenHash string) error {
	_, err := s.db.Exec(
		`UPDATE enroll_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
		time.Now(), tokenHash)
	return err
}
