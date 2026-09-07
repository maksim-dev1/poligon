package runner

import "testing"

func TestScanCrash(t *testing.T) {
	clean := `01-01 10:00:00.000  1000  1000 I ActivityManager: Start proc com.acme.app
01-01 10:00:01.000  1000  1000 I chatty: uid=1000 expire messages`
	if got := scanCrash(clean, "com.acme.app"); got != "" {
		t.Fatalf("clean log flagged: %q", got)
	}

	crashed := `01-01 10:00:00.000  1000  1000 I ActivityManager: Start proc com.acme.app
01-01 10:00:02.500  2000  2000 E AndroidRuntime: FATAL EXCEPTION: main
01-01 10:00:02.500  2000  2000 E AndroidRuntime: Process: com.acme.app, PID: 2000
01-01 10:00:02.500  2000  2000 E AndroidRuntime: java.lang.NullPointerException`
	if got := scanCrash(crashed, "com.acme.app"); got == "" {
		t.Fatal("crash not detected")
	}

	// a different app's crash must not fail our run
	other := `01-01 10:00:02.500  2000  2000 E AndroidRuntime: FATAL EXCEPTION: main
01-01 10:00:02.500  2000  2000 E AndroidRuntime: Process: com.other.thing, PID: 2000`
	if got := scanCrash(other, "com.acme.app"); got != "" {
		t.Fatalf("unrelated crash flagged: %q", got)
	}
}
