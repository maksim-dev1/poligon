package store

import (
	"database/sql"
	"fmt"
)

// TunnelPort returns the user's persistently-assigned adb-tunnel port,
// allocating the next free one in [rangeStart, rangeEnd] on first call. The
// assignment sticks forever (barring a manual DB edit), so a developer's
// ANDROID_ADB_SERVER_PORT never needs to change.
func (s *Store) TunnelPort(user string, rangeStart, rangeEnd int) (int, error) {
	var port int
	err := s.db.QueryRow(`SELECT port FROM adb_tunnel_ports WHERE user_name = ?`, user).Scan(&port)
	if err == nil {
		return port, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	rows, err := s.db.Query(`SELECT port FROM adb_tunnel_ports`)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, err
		}
		used[p] = true
	}
	rows.Close()

	for p := rangeStart; p <= rangeEnd; p++ {
		if !used[p] {
			port = p
			break
		}
	}
	if port == 0 {
		return 0, fmt.Errorf("no free adb tunnel port in [%d, %d]", rangeStart, rangeEnd)
	}
	if _, err := s.db.Exec(`INSERT INTO adb_tunnel_ports (user_name, port) VALUES (?, ?)`, user, port); err != nil {
		return 0, err
	}
	return port, nil
}
