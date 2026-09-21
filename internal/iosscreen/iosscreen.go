// Package iosscreen gives the dashboard a live, controllable view of an iOS
// device via a running WebDriverAgent (WDA):
//
//   - MJPEG frames from WDA's mjpeg server (device port 9100)
//   - tap / swipe / button / text input through WDA's HTTP API (device port 8100)
//
// poligon assumes WDA is already running on the device and its ports forwarded
// to the host (e.g. with `iproxy`). Endpoint addresses come from config
// (ios_screen.<device-id>). Launch orchestration (build + run WDA + iproxy) is
// out of scope here — see scripts/ios-wda.sh.
package iosscreen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Endpoint is where a device's WDA is reachable on the host.
type Endpoint struct {
	WDA   string `yaml:"wda"`   // host:port of WDA HTTP API (device 8100)
	MJPEG string `yaml:"mjpeg"` // host:port of WDA mjpeg server (device 9100)
}

// Tuning is how WDA should encode its mjpeg stream. A zero field leaves that
// setting alone.
type Tuning struct {
	Framerate int
	Quality   int
	Scale     int
}

// Controller manages WDA sessions and proxies for the configured iOS devices.
type Controller struct {
	endpoints map[string]Endpoint // device id -> endpoint
	tune      Tuning

	mu       sync.Mutex
	sessions map[string]string  // device id -> WDA sessionId
	streams  map[string]*stream // device id -> the one shared screen reader
	client   *http.Client
	// screen streams are long-lived, so they cannot use the request client's timeout
	streamClient *http.Client
}

// New builds a Controller. endpoints may be nil/empty (iOS screen disabled).
func New(endpoints map[string]Endpoint, tune Tuning) *Controller {
	if endpoints == nil {
		endpoints = map[string]Endpoint{}
	}
	return &Controller{
		endpoints: endpoints,
		tune:      tune,
		sessions:  map[string]string{},
		streams:   map[string]*stream{},
		client:    &http.Client{Timeout: 10 * time.Second},
		streamClient: &http.Client{
			Transport: &http.Transport{
				DisableCompression: true, // frames are already JPEG
				MaxIdleConns:       8,
			},
		},
	}
}

// Set registers (or replaces) a device's screen endpoint at runtime, used when
// a device is adopted and its WebDriverAgent comes up.
func (c *Controller) Set(deviceID string, ep Endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endpoints[deviceID] = ep
	delete(c.sessions, deviceID) // force a fresh WDA session against the new endpoint
	c.stopStream(deviceID)       // the old reader points at the previous WDA
}

// Unset drops a device's endpoint — call when its WebDriverAgent has gone away,
// so the screen reports "down" instead of serving a stale (possibly wrong)
// endpoint.
func (c *Controller) Unset(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.endpoints, deviceID)
	delete(c.sessions, deviceID)
	c.stopStream(deviceID)
}

// endpoint returns a copy of a device's endpoint under the lock.
func (c *Controller) endpoint(deviceID string) (Endpoint, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ep, ok := c.endpoints[deviceID]
	return ep, ok
}

// Configured reports whether a device has an iOS-screen endpoint.
func (c *Controller) Configured(deviceID string) bool {
	_, ok := c.endpoint(deviceID)
	return ok
}

// Frame returns the newest JPEG the shared reader has. Polling clients
// (Safari, which will not render a multipart <img>) hit this; it costs one map
// lookup, not a fresh connection to the device.
func (c *Controller) Frame(deviceID string) ([]byte, error) {
	sb, err := c.Subscribe(deviceID)
	if err != nil {
		return nil, err
	}
	defer sb.Close()

	if b, age, err := sb.Latest(); err == nil && age < staleAfter {
		return b, nil
	}
	// nothing cached yet (the reader has just started) — wait briefly for the
	// first frame rather than failing the very first poll
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return sb.Next(ctx)
}

// FrameAge reports how old the newest frame is, so the page can tell a live
// screen from a frozen one without guessing from image errors.
func (c *Controller) FrameAge(deviceID string) (time.Duration, error) {
	c.mu.Lock()
	s := c.streams[deviceID]
	c.mu.Unlock()
	if s == nil {
		return 0, errors.New("screen not being watched")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.data == nil {
		if s.err != nil {
			return 0, s.err
		}
		return 0, errors.New("no frame yet")
	}
	return time.Since(s.cur.at), nil
}

// Screenshot grabs a full-resolution PNG straight from WDA. Run artifacts use
// this rather than a frame off the live stream: the stream is deliberately
// scaled down and heavily compressed for the wall, which is the wrong trade for
// a screenshot someone will open to see why a test failed.
func (c *Controller) Screenshot(deviceID string) ([]byte, error) {
	ep, ok := c.endpoint(deviceID)
	if !ok || ep.WDA == "" {
		return nil, fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	resp, err := c.client.Get("http://" + ep.WDA + "/screenshot")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("wda screenshot: %s", resp.Status)
	}
	var out struct {
		Value string `json:"value"` // base64 PNG
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Value == "" {
		return nil, errors.New("wda returned an empty screenshot")
	}
	return base64.StdEncoding.DecodeString(out.Value)
}

// Input is one control action from the dashboard.
type Input struct {
	Type     string  `json:"type"` // tap | swipe | home | text | lock
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	X2       float64 `json:"x2"`
	Y2       float64 `json:"y2"`
	Duration float64 `json:"duration"` // seconds, for swipe
	Text     string  `json:"text"`
}

// Do performs an input action against the device's WDA.
func (c *Controller) Do(deviceID string, in Input) error {
	ep, ok := c.endpoint(deviceID)
	if !ok || ep.WDA == "" {
		return fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	base := "http://" + ep.WDA

	switch in.Type {
	case "home":
		return c.post(base+"/wda/homescreen", nil)
	case "wake":
		// wakes the screen; fully unlocks only if the device has no passcode
		return c.post(base+"/wda/unlock", nil)
	case "text":
		sid, err := c.session(deviceID, base)
		if err != nil {
			return err
		}
		return c.post(fmt.Sprintf("%s/session/%s/wda/keys", base, sid),
			map[string]any{"value": []rune(in.Text)})
	case "tap":
		sid, err := c.session(deviceID, base)
		if err != nil {
			return err
		}
		return c.post(fmt.Sprintf("%s/session/%s/wda/tap", base, sid),
			map[string]any{"x": in.X, "y": in.Y})
	case "swipe":
		sid, err := c.session(deviceID, base)
		if err != nil {
			return err
		}
		dur := in.Duration
		if dur == 0 {
			dur = 0.15
		}
		return c.post(fmt.Sprintf("%s/session/%s/wda/dragfromtoforduration", base, sid),
			map[string]any{
				"fromX": in.X, "fromY": in.Y,
				"toX": in.X2, "toY": in.Y2, "duration": dur,
			})
	default:
		return fmt.Errorf("unknown input type %q", in.Type)
	}
}

// Size returns the device's logical screen size (points).
func (c *Controller) Size(deviceID string) (w, h int, err error) {
	ep, ok := c.endpoint(deviceID)
	if !ok {
		return 0, 0, fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	base := "http://" + ep.WDA
	sid, err := c.session(deviceID, base)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.client.Get(fmt.Sprintf("%s/session/%s/window/size", base, sid))
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Value struct{ Width, Height int } `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, err
	}
	return out.Value.Width, out.Value.Height, nil
}

// ActiveApp returns the bundle id and pid of the app currently in the
// foreground (WDA's own `/wda/activeAppInfo`, no session needed) — used by
// install_smoke to check a just-launched app is actually running rather than
// having crashed back to the springboard.
func (c *Controller) ActiveApp(deviceID string) (bundleID string, pid int, err error) {
	ep, ok := c.endpoint(deviceID)
	if !ok || ep.WDA == "" {
		return "", 0, fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	resp, err := c.client.Get("http://" + ep.WDA + "/wda/activeAppInfo")
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Value struct {
			BundleID string `json:"bundleId"`
			PID      int    `json:"pid"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	return out.Value.BundleID, out.Value.PID, nil
}

// session returns a live WDA sessionId for the device, creating one if needed.
func (c *Controller) session(deviceID, base string) (string, error) {
	c.mu.Lock()
	sid := c.sessions[deviceID]
	c.mu.Unlock()
	if sid != "" && c.sessionAlive(base, sid) {
		return sid, nil
	}

	body, _ := json.Marshal(map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": map[string]any{"platformName": "iOS"},
		},
	})
	resp, err := c.client.Post(base+"/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Value struct {
			SessionID string `json:"sessionId"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Value.SessionID == "" {
		return "", fmt.Errorf("wda returned no sessionId")
	}
	c.mu.Lock()
	c.sessions[deviceID] = out.Value.SessionID
	c.mu.Unlock()
	return out.Value.SessionID, nil
}

func (c *Controller) sessionAlive(base, sid string) bool {
	resp, err := c.client.Get(fmt.Sprintf("%s/session/%s", base, sid))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *Controller) post(u string, payload map[string]any) error {
	var r io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader([]byte("{}"))
	}
	resp, err := c.client.Post(u, "application/json", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("wda %s: %s: %s", u, resp.Status, b)
	}
	return nil
}

// ParseEndpoints normalizes a config map, dropping incomplete entries.
func ParseEndpoints(in map[string]Endpoint) map[string]Endpoint {
	out := map[string]Endpoint{}
	for id, ep := range in {
		if ep.WDA == "" && ep.MJPEG == "" {
			continue
		}
		// default the sibling port if only one was given
		if ep.WDA != "" && ep.MJPEG == "" {
			if h, _, err := net.SplitHostPort(ep.WDA); err == nil {
				ep.MJPEG = net.JoinHostPort(h, "9100")
			}
		}
		if ep.MJPEG != "" && ep.WDA == "" {
			if h, _, err := net.SplitHostPort(ep.MJPEG); err == nil {
				ep.WDA = net.JoinHostPort(h, "8100")
			}
		}
		out[id] = ep
	}
	return out
}
