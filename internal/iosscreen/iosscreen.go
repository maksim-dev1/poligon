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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// Endpoint is where a device's WDA is reachable on the host.
type Endpoint struct {
	WDA   string `yaml:"wda"`   // host:port of WDA HTTP API (device 8100)
	MJPEG string `yaml:"mjpeg"` // host:port of WDA mjpeg server (device 9100)
}

// Controller manages WDA sessions and proxies for the configured iOS devices.
type Controller struct {
	endpoints map[string]Endpoint // device id -> endpoint

	mu       sync.Mutex
	sessions map[string]string // device id -> WDA sessionId
	proxies  map[string]*httputil.ReverseProxy
	client   *http.Client
}

// New builds a Controller. endpoints may be nil/empty (iOS screen disabled).
func New(endpoints map[string]Endpoint) *Controller {
	return &Controller{
		endpoints: endpoints,
		sessions:  map[string]string{},
		proxies:   map[string]*httputil.ReverseProxy{},
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Configured reports whether a device has an iOS-screen endpoint.
func (c *Controller) Configured(deviceID string) bool {
	_, ok := c.endpoints[deviceID]
	return ok
}

// MJPEGHandler proxies the device's mjpeg stream. The caller must have already
// checked auth + reservation.
func (c *Controller) MJPEGHandler(deviceID string) (http.Handler, error) {
	ep, ok := c.endpoints[deviceID]
	if !ok || ep.MJPEG == "" {
		return nil, fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	c.mu.Lock()
	rp := c.proxies[deviceID]
	if rp == nil {
		u := &url.URL{Scheme: "http", Host: ep.MJPEG}
		rp = httputil.NewSingleHostReverseProxy(u)
		rp.FlushInterval = 10 * time.Millisecond // stream frames as they arrive
		c.proxies[deviceID] = rp
	}
	c.mu.Unlock()
	return rp, nil
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
	ep, ok := c.endpoints[deviceID]
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
	ep, ok := c.endpoints[deviceID]
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
