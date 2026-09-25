// Package uitree turns a screen's raw view hierarchy — Android uiautomator XML
// or iOS WebDriverAgent source XML — into a flat list of the elements an agent
// can see and act on, and finds one by its text or id.
package uitree

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Element is one on-screen node. X/Y/W/H are in the platform's native touch
// space: pixels on Android, points on iOS.
type Element struct {
	Index      int
	Class      string // short class/type: Button, EditText, StaticText, …
	Text       string // visible text (Android text, iOS label)
	ID         string // Android resource-id, iOS accessibility identifier (name)
	Desc       string // Android content-desc
	Value      string // iOS value (field contents, switch state)
	X, Y, W, H int
	Clickable  bool
	Scrollable bool
	Checkable  bool
	Checked    bool
	Enabled    bool
	Focused    bool
	Selected   bool
	Editable   bool
}

// Center is the element's tap point.
func (e Element) Center() (int, int) { return e.X + e.W/2, e.Y + e.H/2 }

// Parse reads either dump format, detected from its root.
func Parse(raw string) ([]Element, error) {
	if strings.Contains(raw, "<hierarchy") {
		return parseAndroid(raw)
	}
	if strings.Contains(raw, "XCUIElementType") {
		return parseIOS(raw)
	}
	return nil, errors.New("unrecognized UI dump")
}

var boundsRe = regexp.MustCompile(`^\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]$`)

func parseAndroid(raw string) ([]Element, error) {
	var out []Element
	err := walk(raw, func(se xml.StartElement) {
		if se.Name.Local != "node" {
			return
		}
		a := attrs(se)
		m := boundsRe.FindStringSubmatch(a["bounds"])
		if m == nil {
			return
		}
		x1, _ := strconv.Atoi(m[1])
		y1, _ := strconv.Atoi(m[2])
		x2, _ := strconv.Atoi(m[3])
		y2, _ := strconv.Atoi(m[4])
		cls := a["class"]
		if i := strings.LastIndexByte(cls, '.'); i >= 0 {
			cls = cls[i+1:]
		}
		id := a["resource-id"]
		if i := strings.Index(id, ":id/"); i >= 0 {
			id = id[i+4:]
		}
		out = append(out, Element{
			Class: cls, Text: a["text"], ID: id, Desc: a["content-desc"],
			X: x1, Y: y1, W: x2 - x1, H: y2 - y1,
			Clickable:  a["clickable"] == "true" || a["long-clickable"] == "true",
			Scrollable: a["scrollable"] == "true",
			Checkable:  a["checkable"] == "true",
			Checked:    a["checked"] == "true",
			Enabled:    a["enabled"] != "false",
			Focused:    a["focused"] == "true",
			Selected:   a["selected"] == "true",
			Editable:   strings.Contains(cls, "EditText") || a["password"] == "true",
		})
	})
	return number(out), err
}

// iOS element types that respond to a tap even though WDA exposes no
// "clickable" flag.
var iosTappable = map[string]bool{
	"Button": true, "Cell": true, "Link": true, "Switch": true, "Toggle": true,
	"TextField": true, "SecureTextField": true, "SearchField": true, "TextView": true,
	"Tab": true, "SegmentedControl": true, "Slider": true, "Stepper": true,
	"Picker": true, "PickerWheel": true, "MenuItem": true, "Key": true, "Icon": true,
	"CheckBox": true, "RadioButton": true, "PageIndicator": true,
}

func parseIOS(raw string) ([]Element, error) {
	var out []Element
	err := walk(raw, func(se xml.StartElement) {
		typ, ok := strings.CutPrefix(se.Name.Local, "XCUIElementType")
		if !ok || typ == "Application" || typ == "Window" {
			return
		}
		a := attrs(se)
		if a["visible"] == "false" {
			return
		}
		num := func(k string) int { n, _ := strconv.ParseFloat(a[k], 64); return int(n) }
		label, name := a["label"], a["name"]
		e := Element{
			Class: typ, Text: label, Value: a["value"],
			X: num("x"), Y: num("y"), W: num("width"), H: num("height"),
			Clickable:  iosTappable[typ],
			Scrollable: typ == "ScrollView" || typ == "Table" || typ == "CollectionView" || typ == "WebView",
			Checkable:  typ == "Switch" || typ == "Toggle",
			Checked:    (typ == "Switch" || typ == "Toggle") && a["value"] == "1",
			Enabled:    a["enabled"] != "false",
			Focused:    a["hasFocus"] == "true" || a["focused"] == "true",
			Selected:   a["selected"] == "true",
			Editable:   typ == "TextField" || typ == "SecureTextField" || typ == "SearchField" || typ == "TextView",
		}
		// name falls back to the label when no identifier is set; only a
		// distinct name is a real accessibility id
		if name != label {
			e.ID = name
		}
		if typ == "StaticText" && e.Text == "" {
			e.Text = e.Value
		}
		if e.Value == e.Text {
			e.Value = ""
		}
		out = append(out, e)
	})
	return number(out), err
}

func walk(raw string, fn func(xml.StartElement)) error {
	d := xml.NewDecoder(strings.NewReader(raw))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parse UI dump: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			fn(se)
		}
	}
}

func attrs(se xml.StartElement) map[string]string {
	m := make(map[string]string, len(se.Attr))
	for _, a := range se.Attr {
		m[a.Name.Local] = a.Value
	}
	return m
}

func number(els []Element) []Element {
	for i := range els {
		els[i].Index = i
	}
	return els
}

// Interesting keeps what an agent can read or act on: anything with text, an
// id or a description, plus every tappable, scrollable or editable node — and
// drops zero-size and fully off-screen nodes (screenW/H 0 = unknown, skip that
// check). Indexes are kept, so a filtered element still resolves by index.
func Interesting(els []Element, screenW, screenH int) []Element {
	var out []Element
	for _, e := range els {
		if e.W <= 0 || e.H <= 0 {
			continue
		}
		if screenW > 0 && screenH > 0 && (e.X >= screenW || e.Y >= screenH || e.X+e.W <= 0 || e.Y+e.H <= 0) {
			continue
		}
		if e.Text != "" || e.ID != "" || e.Desc != "" || e.Value != "" ||
			e.Clickable || e.Scrollable || e.Editable || e.Checkable {
			out = append(out, e)
		}
	}
	return out
}

// Query picks an element: by index, or by text (matched against text,
// description and value) and/or id.
type Query struct {
	Index *int
	Text  string
	ID    string
}

func (q Query) String() string {
	var p []string
	if q.Index != nil {
		p = append(p, fmt.Sprintf("index=%d", *q.Index))
	}
	if q.Text != "" {
		p = append(p, fmt.Sprintf("text=%q", q.Text))
	}
	if q.ID != "" {
		p = append(p, fmt.Sprintf("id=%q", q.ID))
	}
	return strings.Join(p, " ")
}

// Empty reports whether the query names nothing.
func (q Query) Empty() bool { return q.Index == nil && q.Text == "" && q.ID == "" }

// Find returns the best match and how many elements matched. Exact
// (case-insensitive) matches win over substring ones; among equals a tappable
// element wins, then the first in document order.
func Find(els []Element, q Query) (Element, int, bool) {
	if q.Index != nil {
		for _, e := range els {
			if e.Index == *q.Index {
				return e, 1, true
			}
		}
		return Element{}, 0, false
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	text, id := norm(q.Text), norm(q.ID)
	idOK := func(e Element) bool { return id == "" || norm(e.ID) == id }
	var exact, partial []Element
	for _, e := range els {
		if e.W <= 0 || e.H <= 0 || !idOK(e) {
			continue
		}
		if text == "" {
			exact = append(exact, e)
			continue
		}
		fields := []string{norm(e.Text), norm(e.Desc), norm(e.Value)}
		switch {
		case slices.ContainsFunc(fields, func(f string) bool { return f == text }):
			exact = append(exact, e)
		case slices.ContainsFunc(fields, func(f string) bool { return f != "" && strings.Contains(f, text) }):
			partial = append(partial, e)
		}
	}
	pick := exact
	if len(pick) == 0 {
		pick = partial
	}
	if len(pick) == 0 {
		return Element{}, 0, false
	}
	for _, e := range pick {
		if e.Clickable && e.Enabled {
			return e, len(pick), true
		}
	}
	return pick[0], len(pick), true
}

// Format renders one element as a single line for an agent; scale maps
// native coordinates into the caller's coordinate space.
func Format(e Element, scale float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%d] %s", e.Index, e.Class)
	if e.Text != "" {
		fmt.Fprintf(&b, " %q", clip(e.Text))
	}
	if e.Desc != "" && e.Desc != e.Text {
		fmt.Fprintf(&b, " desc=%q", clip(e.Desc))
	}
	if e.Value != "" {
		fmt.Fprintf(&b, " value=%q", clip(e.Value))
	}
	if e.ID != "" {
		fmt.Fprintf(&b, " id=%s", e.ID)
	}
	cx, cy := e.Center()
	fmt.Fprintf(&b, " @(%d,%d) %dx%d", sc(cx, scale), sc(cy, scale), sc(e.W, scale), sc(e.H, scale))
	var flags []string
	add := func(on bool, f string) {
		if on {
			flags = append(flags, f)
		}
	}
	add(e.Clickable, "tap")
	add(e.Editable, "edit")
	add(e.Scrollable, "scroll")
	add(e.Checkable && e.Checked, "checked")
	add(e.Checkable && !e.Checked, "unchecked")
	add(e.Focused, "focused")
	add(e.Selected, "selected")
	add(!e.Enabled, "disabled")
	if len(flags) > 0 {
		b.WriteString(" " + strings.Join(flags, ","))
	}
	return b.String()
}

func sc(v int, scale float64) int { return int(float64(v)*scale + 0.5) }

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}
