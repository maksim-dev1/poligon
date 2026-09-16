package adbfilter

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeADBServer answers host:devices and host:transport:<serial> like a real
// adb server, for two fixed serials.
func fakeADBServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				msg, err := readMsg(conn)
				if err != nil {
					return
				}
				switch {
				case msg == "host:devices":
					_, _ = conn.Write([]byte("OKAY"))
					_ = writeMsg(conn, "serial-a\tdevice\nserial-b\tdevice\n")
				case strings.HasPrefix(msg, "host:transport:"):
					serial := strings.TrimPrefix(msg, "host:transport:")
					if serial == "serial-a" || serial == "serial-b" {
						_, _ = conn.Write([]byte("OKAY"))
						buf := bufio.NewReader(conn)
						line, _ := buf.ReadString('\n')
						_, _ = conn.Write([]byte("echo:" + line))
					} else {
						_, _ = conn.Write([]byte("FAIL"))
						_ = writeMsg(conn, "device not found")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func dialAndSend(t *testing.T, addr, msg string) (status string, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeMsg(conn, msg); err != nil {
		t.Fatal(err)
	}
	var st [4]byte
	if _, err := io.ReadFull(conn, st[:]); err != nil {
		t.Fatal(err)
	}
	p, _ := readMsg(conn)
	return string(st[:]), p
}

func TestFilterDevicesList(t *testing.T) {
	upstream := fakeADBServer(t)
	m := New(upstream)
	if err := m.Ensure("alice", 0); err != nil {
		t.Fatal(err)
	}
	// Ensure with port 0 lets the OS pick a free port; find it back out.
	m.mu.Lock()
	addr := m.tunnels["alice"].ln.Addr().String()
	m.mu.Unlock()

	m.SetAllowed("alice", []string{"serial-a"})

	status, payload := dialAndSend(t, addr, "host:devices")
	if status != "OKAY" {
		t.Fatalf("status = %q", status)
	}
	if payload != "serial-a\tdevice\n" {
		t.Fatalf("payload = %q, want only serial-a", payload)
	}
}

func TestTransportGating(t *testing.T) {
	upstream := fakeADBServer(t)
	m := New(upstream)
	if err := m.Ensure("bob", 0); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	addr := m.tunnels["bob"].ln.Addr().String()
	m.mu.Unlock()
	m.SetAllowed("bob", []string{"serial-a"})

	// allowed serial: transport succeeds, then raw bytes splice through.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeMsg(conn, "host:transport:serial-a"); err != nil {
		t.Fatal(err)
	}
	var st [4]byte
	if _, err := io.ReadFull(conn, st[:]); err != nil {
		t.Fatal(err)
	}
	if string(st[:]) != "OKAY" {
		t.Fatalf("status = %q, want OKAY", st)
	}
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	buf := bufio.NewReader(conn)
	line, err := buf.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "echo:hello\n" {
		t.Fatalf("splice roundtrip = %q", line)
	}

	// not-allowed serial: rejected before ever touching upstream.
	conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	if err := writeMsg(conn2, "host:transport:serial-b"); err != nil {
		t.Fatal(err)
	}
	var st2 [4]byte
	if _, err := io.ReadFull(conn2, st2[:]); err != nil {
		t.Fatal(err)
	}
	if string(st2[:]) != "FAIL" {
		t.Fatalf("status = %q, want FAIL for unreserved device", st2)
	}
}
