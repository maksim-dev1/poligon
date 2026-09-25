package uitree

import (
	"strings"
	"testing"
)

const androidDump = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
  <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="app" content-desc="" clickable="false" enabled="true" bounds="[0,0][1080,2400]">
    <node index="0" text="Войти" resource-id="app:id/login_title" class="android.widget.TextView" package="app" content-desc="" clickable="false" enabled="true" bounds="[100,200][980,300]" />
    <node index="1" text="" resource-id="app:id/email" class="android.widget.EditText" package="app" content-desc="" clickable="true" enabled="true" focused="true" bounds="[100,400][980,520]" />
    <node index="2" text="Войти" resource-id="app:id/login_btn" class="android.widget.Button" package="app" content-desc="" clickable="true" enabled="true" bounds="[100,1800][980,1920]" />
    <node index="3" text="" resource-id="" class="android.view.View" package="app" content-desc="" clickable="false" enabled="true" bounds="[0,0][0,0]" />
    <node index="4" text="Remember me" resource-id="" class="android.widget.CheckBox" checkable="true" checked="true" clickable="true" enabled="true" bounds="[100,600][500,680]" />
  </node>
</hierarchy>`

const iosSource = `<?xml version="1.0" encoding="UTF-8"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="Demo" label="Demo" enabled="true" visible="true" x="0" y="0" width="390" height="844">
  <XCUIElementTypeWindow type="XCUIElementTypeWindow" enabled="true" visible="true" x="0" y="0" width="390" height="844">
    <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" value="Welcome" name="Welcome" label="Welcome" enabled="true" visible="true" x="20" y="100" width="350" height="30"/>
    <XCUIElementTypeTextField type="XCUIElementTypeTextField" value="" name="emailField" label="Email" enabled="true" visible="true" x="20" y="200" width="350" height="44"/>
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="Sign in" label="Sign in" enabled="true" visible="true" x="20" y="700" width="350" height="50"/>
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="hidden" label="hidden" enabled="true" visible="false" x="20" y="900" width="10" height="10"/>
    <XCUIElementTypeSwitch type="XCUIElementTypeSwitch" value="1" name="notify" label="Notifications" enabled="true" visible="true" x="300" y="400" width="51" height="31"/>
  </XCUIElementTypeWindow>
</XCUIElementTypeApplication>`

func TestAndroid(t *testing.T) {
	els, err := Parse(androidDump)
	if err != nil {
		t.Fatal(err)
	}
	if len(els) != 6 {
		t.Fatalf("got %d elements", len(els))
	}
	vis := Interesting(els, 1080, 2400)
	if len(vis) != 4 { // title, email, button, checkbox — not the bare frame or zero-size view
		t.Fatalf("interesting: %d", len(vis))
	}

	// "Войти" appears twice: the tappable button wins over the title
	e, n, ok := Find(els, Query{Text: "войти"})
	if !ok || n != 2 || e.ID != "login_btn" {
		t.Fatalf("find text: %+v n=%d ok=%v", e, n, ok)
	}
	if x, y := e.Center(); x != 540 || y != 1860 {
		t.Fatalf("center %d,%d", x, y)
	}
	if e, _, ok := Find(els, Query{ID: "email"}); !ok || !e.Editable || !e.Focused {
		t.Fatalf("find id: %+v", e)
	}
	cb, _, ok := Find(els, Query{Text: "remember"})
	if !ok || !cb.Checked {
		t.Fatalf("substring: %+v", cb)
	}
	if _, _, ok := Find(els, Query{Text: "nope"}); ok {
		t.Fatal("found a missing element")
	}

	line := Format(cb, 0.5)
	for _, want := range []string{`CheckBox "Remember me"`, "@(150,320)", "200x40", "tap", "checked"} {
		if !strings.Contains(line, want) {
			t.Errorf("format %q missing %q", line, want)
		}
	}
}

func TestIOS(t *testing.T) {
	els, err := Parse(iosSource)
	if err != nil {
		t.Fatal(err)
	}
	if len(els) != 4 { // application, window and the invisible button dropped
		t.Fatalf("got %d elements: %+v", len(els), els)
	}
	e, _, ok := Find(els, Query{Text: "sign in"})
	if !ok || !e.Clickable || e.ID != "" {
		t.Fatalf("button: %+v", e)
	}
	f, _, ok := Find(els, Query{ID: "emailField"})
	if !ok || !f.Editable || f.Text != "Email" {
		t.Fatalf("field: %+v", f)
	}
	s, _, _ := Find(els, Query{ID: "notify"})
	if !s.Checked {
		t.Fatalf("switch: %+v", s)
	}
	idx := 0
	if w, _, ok := Find(els, Query{Index: &idx}); !ok || w.Text != "Welcome" || w.Value != "" {
		t.Fatalf("index: %+v", w)
	}
}
