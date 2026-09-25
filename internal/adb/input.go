package adb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tap touches one point of the screen (device pixels).
func (a *ADB) Tap(ctx context.Context, serial string, x, y int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.shell(ctx, serial, "input", "tap", fmt.Sprint(x), fmt.Sprint(y))
	return err
}

// Swipe drags from one point to another over ms milliseconds. A swipe that
// starts and ends on the same point is a long press.
func (a *ADB) Swipe(ctx context.Context, serial string, x1, y1, x2, y2, ms int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second+time.Duration(ms)*time.Millisecond)
	defer cancel()
	_, err := a.shell(ctx, serial, "input", "swipe",
		fmt.Sprint(x1), fmt.Sprint(y1), fmt.Sprint(x2), fmt.Sprint(y2), fmt.Sprint(ms))
	return err
}

// ErrNonASCII is returned by InputText for text `input text` cannot type.
var ErrNonASCII = errors.New("adb `input text` types printable ASCII only (no Cyrillic/emoji)")

// InputText types text into the focused field. `input text` knows printable
// ASCII only, reads "%s" as a space, and cannot press Enter — so spaces are
// escaped and each newline becomes a KEYCODE_ENTER between typed chunks.
func (a *ADB) InputText(ctx context.Context, serial, text string) error {
	for _, r := range text {
		if r != '\n' && (r < 0x20 || r > 0x7e) {
			return ErrNonASCII
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for i, chunk := range strings.Split(text, "\n") {
		if i > 0 {
			if _, err := a.shell(ctx, serial, "input", "keyevent", "66"); err != nil {
				return err
			}
		}
		if chunk == "" {
			continue
		}
		if _, err := a.shell(ctx, serial, "input", "text", shQuote(strings.ReplaceAll(chunk, " ", "%s"))); err != nil {
			return err
		}
	}
	return nil
}

// OpenURL fires an ACTION_VIEW intent — a deep link or a web page.
func (a *ADB) OpenURL(ctx context.Context, serial, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := a.shell(ctx, serial, "am", "start", "-a", "android.intent.action.VIEW", "-d", shQuote(url))
	if err == nil && strings.Contains(out, "Error:") {
		err = fmt.Errorf("am start: %s", strings.TrimSpace(out))
	}
	return err
}

// Exec runs one shell command line on the device and returns its combined
// output. The line goes to the device's /system/bin/sh as-is — callers pass
// what a holder could equally type into the dashboard's terminal.
func (a *ADB) Exec(ctx context.Context, serial, command string) (string, error) {
	out, err := a.shell(ctx, serial, command)
	return out, err
}

// ClearField jumps to the end of the focused field and deletes n characters.
func (a *ADB) ClearField(ctx context.Context, serial string, n int) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := a.shell(ctx, serial, "input", "keyevent", "123"); err != nil { // MOVE_END
		return err
	}
	_, err := a.shell(ctx, serial, "input keyevent"+strings.Repeat(" 67", n)) // DEL ×n
	return err
}
