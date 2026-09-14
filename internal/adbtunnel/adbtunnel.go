// Package adbtunnel exposes the farm host's local adb server on the network
// so a developer's own `adb`/`flutter run`/VS Code can attach live to a farm
// device — hot reload, breakpoints, the works — instead of only the
// install-and-collect-artifacts flow the rest of poligon offers.
//
// adb's wire protocol has no per-device access control: once the tunnel is
// open, every device currently attached to the host is reachable through it,
// regardless of who holds what in poligon's own reservation system. That is
// not a new class of exposure for this project (open registration, "the
// network is the perimeter" — see the auth package), but it is coarser than
// every other poligon capability, which is why the tunnel is closed by
// default, opened only on request, and closes itself after a period with no
// keepalive ping.
package adbtunnel

import (
	"io"
	"net"
	"sync"
	"time"
)

// Manager runs at most one tunnel at a time — a single TCP listener proxying
// byte-for-byte to the local adb server.
type Manager struct {
	upstream string // adb server address to forward to, e.g. "127.0.0.1:5037"

	mu       sync.Mutex
	ln       net.Listener
	lastPing time.Time
}

// New builds a Manager forwarding to upstream (defaults to the standard
// local adb server address if empty) and starts its own idle-reaper — no
// caller-managed lifecycle needed, it just closes the tunnel after idle with
// no Ping/Start, for as long as the poligon process runs.
func New(upstream string, idle time.Duration) *Manager {
	if upstream == "" {
		upstream = "127.0.0.1:5037"
	}
	if idle <= 0 {
		idle = 15 * time.Minute
	}
	m := &Manager{upstream: upstream}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			m.ReapIdle(idle)
		}
	}()
	return m
}

// Start opens the listener on listenAddr (e.g. ":5038") if it isn't already
// running, and returns the port it's listening on. Calling it again while
// already running is a no-op that also counts as a keepalive.
func (m *Manager) Start(listenAddr string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPing = time.Now()
	if m.ln != nil {
		return m.ln.Addr().(*net.TCPAddr).Port, nil
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return 0, err
	}
	m.ln = ln
	go m.acceptLoop(ln)
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (m *Manager) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (Stop, or ReapIdle)
		}
		go m.relay(conn)
	}
}

func (m *Manager) relay(conn net.Conn) {
	defer conn.Close()
	upstream, err := net.DialTimeout("tcp", m.upstream, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

// Ping is a keepalive — call it periodically while a client wants the tunnel
// to stay open (see ReapIdle).
func (m *Manager) Ping() {
	m.mu.Lock()
	m.lastPing = time.Now()
	m.mu.Unlock()
}

// Running reports whether the tunnel is open, and its port.
func (m *Manager) Running() (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return false, 0
	}
	return true, m.ln.Addr().(*net.TCPAddr).Port
}

// Stop closes the tunnel immediately.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln != nil {
		_ = m.ln.Close()
		m.ln = nil
	}
}

// ReapIdle closes the tunnel if its last Ping (or Start) was longer than idle
// ago. Call from a periodic loop — mirrors reserve.Manager's own idle-release
// ticker.
func (m *Manager) ReapIdle(idle time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln != nil && time.Since(m.lastPing) > idle {
		_ = m.ln.Close()
		m.ln = nil
	}
}
