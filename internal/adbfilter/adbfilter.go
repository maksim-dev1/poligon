// Package adbfilter exposes the farm host's local adb server on the network,
// same as internal/adbtunnel, but per user: each developer gets their own
// TCP port that only ever shows the Android devices they currently hold a
// reservation for — not the whole farm.
//
// It works by speaking just enough of adb's "smart socket" wire protocol
// (4-hex-digit length prefix + string) to intercept the two request kinds
// that enumerate or select a device — host:devices(-l), host:track-devices(-l)
// and host:transport:<serial> — filtering/gating them against a live
// allow-list, and otherwise relaying every other request byte-for-byte to the
// real adb server exactly like a dumb proxy would.
package adbfilter

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager runs one listener per user, all forwarding to the same upstream adb
// server.
type Manager struct {
	upstream string

	mu      sync.Mutex
	tunnels map[string]*userTunnel
}

type userTunnel struct {
	ln net.Listener

	mu      sync.Mutex
	allowed map[string]bool
}

// New builds a Manager forwarding to upstream (defaults to the standard local
// adb server address if empty).
func New(upstream string) *Manager {
	if upstream == "" {
		upstream = "127.0.0.1:5037"
	}
	return &Manager{upstream: upstream, tunnels: map[string]*userTunnel{}}
}

// Ensure opens user's listener on port if it isn't already running. Calling
// it again for a user that's already listening is a no-op.
func (m *Manager) Ensure(user string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tunnels[user]; ok {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	t := &userTunnel{ln: ln, allowed: map[string]bool{}}
	m.tunnels[user] = t
	go m.acceptLoop(t)
	return nil
}

// SetAllowed replaces the set of ADB serials visible to user. A user with no
// listener yet (never called Ensure) is a no-op — call Ensure first.
func (m *Manager) SetAllowed(user string, serials []string) {
	m.mu.Lock()
	t := m.tunnels[user]
	m.mu.Unlock()
	if t == nil {
		return
	}
	allowed := make(map[string]bool, len(serials))
	for _, s := range serials {
		allowed[s] = true
	}
	t.mu.Lock()
	t.allowed = allowed
	t.mu.Unlock()
}

// Users lists everyone with an open listener, for a periodic allow-list
// refresh (reservations expire silently, not just via an explicit release).
func (m *Manager) Users() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.tunnels))
	for u := range m.tunnels {
		out = append(out, u)
	}
	return out
}

func (m *Manager) acceptLoop(t *userTunnel) {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go m.handle(t, conn)
	}
}

func (m *Manager) handle(t *userTunnel, conn net.Conn) {
	defer conn.Close()
	msg, err := readMsg(conn)
	if err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", m.upstream, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	switch {
	case strings.HasPrefix(msg, "host:transport:"):
		serial := strings.TrimPrefix(msg, "host:transport:")
		t.mu.Lock()
		ok := t.allowed[serial]
		t.mu.Unlock()
		if !ok {
			_ = writeFail(conn, "device not reserved by you on this farm")
			return
		}
		if err := writeMsg(upstream, msg); err != nil {
			return
		}
		if !relayStatus(upstream, conn) {
			return
		}
		splice(conn, upstream)

	case msg == "host:devices" || msg == "host:devices-l":
		if err := writeMsg(upstream, msg); err != nil {
			return
		}
		filterOnce(upstream, conn, t)

	case msg == "host:track-devices" || msg == "host:track-devices-l":
		if err := writeMsg(upstream, msg); err != nil {
			return
		}
		filterTrack(upstream, conn, t)

	default:
		// anything else (host:version, host:kill, per-serial host-serial:...
		// commands used by less common tooling, etc.) passes through raw —
		// none of it enumerates or selects a device on its own.
		if err := writeMsg(upstream, msg); err != nil {
			return
		}
		splice(conn, upstream)
	}
}

// readMsg reads one adb smart-socket request/payload: a 4-hex-digit length
// prefix followed by that many bytes.
func readMsg(r io.Reader) (string, error) {
	var lenHex [4]byte
	if _, err := io.ReadFull(r, lenHex[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(lenHex[:]), 16, 32)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeMsg(w io.Writer, s string) error {
	_, err := fmt.Fprintf(w, "%04x%s", len(s), s)
	return err
}

func writeFail(w io.Writer, reason string) error {
	if _, err := io.WriteString(w, "FAIL"); err != nil {
		return err
	}
	return writeMsg(w, reason)
}

// relayStatus copies the 4-byte OKAY/FAIL status (and, on FAIL, its message)
// from upstream to client. Returns true only on OKAY.
func relayStatus(upstream io.Reader, client io.Writer) bool {
	var status [4]byte
	if _, err := io.ReadFull(upstream, status[:]); err != nil {
		return false
	}
	if _, err := client.Write(status[:]); err != nil {
		return false
	}
	if string(status[:]) != "OKAY" {
		if msg, err := readMsg(upstream); err == nil {
			_ = writeMsg(client, msg)
		}
		return false
	}
	return true
}

// filterOnce relays a single host:devices(-l) response, dropping lines for
// serials not in t's allow-list.
func filterOnce(upstream io.Reader, client io.Writer, t *userTunnel) {
	if !relayStatus(upstream, client) {
		return
	}
	payload, err := readMsg(upstream)
	if err != nil {
		return
	}
	_ = writeMsg(client, filterDeviceList(payload, t))
}

// filterTrack relays host:track-devices(-l): one OKAY, then a stream of
// length-prefixed payloads (no further status bytes) until the connection
// closes, filtering each one.
func filterTrack(upstream io.Reader, client io.Writer, t *userTunnel) {
	var status [4]byte
	if _, err := io.ReadFull(upstream, status[:]); err != nil {
		return
	}
	if _, err := client.Write(status[:]); err != nil {
		return
	}
	if string(status[:]) != "OKAY" {
		if msg, err := readMsg(upstream); err == nil {
			_ = writeMsg(client, msg)
		}
		return
	}
	for {
		payload, err := readMsg(upstream)
		if err != nil {
			return
		}
		if err := writeMsg(client, filterDeviceList(payload, t)); err != nil {
			return
		}
	}
}

// filterDeviceList keeps only the lines (each "serial\tstate ...") whose
// serial is in t's allow-list.
func filterDeviceList(payload string, t *userTunnel) string {
	t.mu.Lock()
	allowed := t.allowed
	t.mu.Unlock()

	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader(payload))
	for sc.Scan() {
		line := sc.Text()
		serial, _, _ := strings.Cut(line, "\t")
		if allowed[serial] {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// splice relays raw bytes both ways until either side closes — used once a
// connection is past the point where we need to interpret it (an approved
// host:transport:<serial>, or any request we don't filter at all).
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
