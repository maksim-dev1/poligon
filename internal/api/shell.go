package api

import (
	"context"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
)

// shellUpgrader upgrades a device-shell request to a WebSocket. GET requests
// skip poligon's CSRF check (see auth.safeMethod) and a browser attaches
// cookies to a cross-origin WebSocket handshake regardless of CORS, so a
// same-origin check here stands in for CSRF protection on this one route —
// this is a live interactive shell on real hardware, worth the extra guard.
var shellUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser client (curl, a test) — no ambient cookie risk
		}
		u, err := url.Parse(origin)
		return err == nil && u.Host == r.Host
	},
}

// wsWriter adapts a *websocket.Conn to io.Writer, serialising writes — cmd's
// Stdout/Stderr copiers can otherwise call Write from two goroutines at once,
// which gorilla/websocket does not allow.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// deviceShell bridges a WebSocket to an interactive `adb shell -tt` on the
// device — a real terminal (job control, colors, vim/top all work) rather
// than one-shot command execution. No resize plumbing yet: the remote PTY
// keeps whatever size it started with.
func (s *Server) deviceShell(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.heldDevice(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := s.capt.ShellCommand(ctx, dev)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	conn, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	out := &wsWriter{conn: conn}
	cmd.Stdout, cmd.Stderr = out, out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n["+err.Error()+"]\r\n"))
		return
	}
	if err := cmd.Start(); err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n["+err.Error()+"]\r\n"))
		return
	}

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				_ = stdin.Close()
				return
			}
			if _, err := stdin.Write(data); err != nil {
				return
			}
		}
	}()

	<-done
}
