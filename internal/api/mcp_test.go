package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/capture"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/store"
)

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpFixture is a farm with one Android phone whose adb is `true`: every
// device command succeeds and prints nothing.
func mcpFixture(t *testing.T) (*Server, *httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, u := range []string{"agent@x.io", "other@x.io"} {
		if err := st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("plgn_" + u))
		if err := st.CreateAPIToken(hex.EncodeToString(sum[:]), u, "t"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"pixel-1", "pixel-2"} {
		if err := st.UpsertDeviceInventory(model.Device{ID: id, Platform: model.Android, Serial: id}); err != nil {
			t.Fatal(err)
		}
		_ = st.SetDeviceStatus(id, model.StatusFree, time.Now())
	}
	res := reserve.New(st, st.DB(), 15*time.Minute, 4*time.Hour)
	s := &Server{
		cfg: config.Config{StorageDir: dir}, st: st, res: res,
		capt: capture.New(adb.New("true"), nil, ios.Tools{}), log: slog.New(slog.DiscardHandler),
	}
	a := auth.New(st, auth.Options{Log: slog.New(slog.DiscardHandler)})
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.mcpHandler(a))
	api := http.NewServeMux()
	api.HandleFunc("POST /uploads", s.createUpload)
	mux.Handle("/api/", http.StripPrefix("/api", a.Middleware(api)))
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return s, hs, st
}

func connect(t *testing.T, url, user string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{"plgn_" + user}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), r.IsError
}

func TestMCPRejectsWithoutToken(t *testing.T) {
	_, hs, _ := mcpFixture(t)
	resp, err := http.Post(hs.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestMCPDeviceFlow(t *testing.T) {
	s, hs, _ := mcpFixture(t)
	cs := connect(t, hs.URL, "agent@x.io")

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 20 {
		t.Fatalf("only %d tools", len(tools.Tools))
	}
	// the embedded selector must flatten into tap's own properties
	for _, tl := range tools.Tools {
		if tl.Name == "tap" {
			raw, _ := json.Marshal(tl.InputSchema)
			for _, want := range []string{`"text"`, `"index"`, `"hold_ms"`} {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("tap schema lacks %s: %s", want, raw)
				}
			}
		}
	}

	if out, isErr := call(t, cs, "tap", map[string]any{"device_id": "pixel-1", "x": 10, "y": 10}); !isErr || !strings.Contains(out, "reserve_device") {
		t.Fatalf("tap before reserve: %q err=%v", out, isErr)
	}
	if out, isErr := call(t, cs, "reserve_device", map[string]any{"device_id": "pixel-1"}); isErr {
		t.Fatalf("reserve: %s", out)
	}
	if out, _ := call(t, cs, "list_devices", nil); !strings.Contains(out, "pixel-1  android") || !strings.Contains(out, "held by you") {
		t.Fatalf("list: %q", out)
	}

	// a 2160-px-tall phone is shown at 1280: image space is 0.5926× native
	s.setNativeSize("pixel-1", 1080, 2160)
	if out, isErr := call(t, cs, "tap", map[string]any{"device_id": "pixel-1", "x": 320, "y": 640}); isErr || !strings.Contains(out, "tapped (320,640)") {
		t.Fatalf("tap: %q", out)
	}
	if out, isErr := call(t, cs, "swipe", map[string]any{"device_id": "pixel-1", "direction": "up"}); isErr || !strings.Contains(out, "(320,960) → (320,320)") {
		t.Fatalf("swipe: %q", out)
	}
	if out, isErr := call(t, cs, "type_text", map[string]any{"device_id": "pixel-1", "text": "привет"}); !isErr || !strings.Contains(out, "ASCII") {
		t.Fatalf("cyrillic on android: %q", out)
	}

	// another user can neither drive nor release it
	other := connect(t, hs.URL, "other@x.io")
	if out, isErr := call(t, other, "press_key", map[string]any{"device_id": "pixel-1", "key": "home"}); !isErr || !strings.Contains(out, "held by agent@x.io") {
		t.Fatalf("other user: %q", out)
	}
	if _, isErr := call(t, other, "release_device", map[string]any{"device_id": "pixel-1"}); !isErr {
		t.Fatal("other user released it")
	}
	if out, isErr := call(t, cs, "release_device", map[string]any{"device_id": "pixel-1"}); isErr {
		t.Fatalf("release: %s", out)
	}
}

func TestStagedUploadIsPrivate(t *testing.T) {
	s, hs, _ := mcpFixture(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "app.apk")
	_, _ = fw.Write([]byte("PK fake"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/uploads", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer plgn_agent@x.io")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var up struct {
		ID string `json:"upload_id"`
	}
	if err := json.Unmarshal(raw, &up); err != nil || !uploadIDRe.MatchString(up.ID) {
		t.Fatalf("upload: %s", raw)
	}
	if p, err := s.stagedUpload("agent@x.io", up.ID); err != nil || filepath.Base(p) != "app.apk" {
		t.Fatalf("owner: %q %v", p, err)
	}
	if _, err := s.stagedUpload("other@x.io", up.ID); err == nil {
		t.Fatal("another user resolved the upload")
	}
	if _, err := s.stagedUpload("agent@x.io", "../../etc"); err == nil {
		t.Fatal("path id accepted")
	}
}
