package adb

import (
	"errors"
	"strings"
	"testing"
)

func TestInstallHint(t *testing.T) {
	out := "adb: failed to install app.apk: Failure [INSTALL_FAILED_USER_RESTRICTED: Install canceled by user]"
	if h := InstallHint(out); !strings.Contains(h, "Install via USB") {
		t.Fatalf("no MIUI hint: %q", h)
	}
	if h := InstallHint("Performing Streamed Install\nSuccess"); h != "" {
		t.Fatalf("hint on success: %q", h)
	}
}

func TestWithInstallHintKeepsError(t *testing.T) {
	base := errors.New("exit status 1: Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]")
	_, err := withInstallHint("", base)
	if !errors.Is(err, base) || !strings.Contains(err.Error(), "hint: INSTALL_FAILED_INSUFFICIENT_STORAGE") {
		t.Fatalf("got %v", err)
	}
	if _, err := withInstallHint("ok", nil); err != nil {
		t.Fatalf("nil error became %v", err)
	}
}
