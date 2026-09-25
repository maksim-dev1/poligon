package main

import (
	"slices"
	"testing"
)

// The state the farm was found in: two fork-servers from different days, a
// hung `adb kill-server`, and clients that must not count as servers.
func TestParseADBServers(t *testing.T) {
	ps := `  58069 adb -L tcp:5037 fork-server server --reply-fd 4
  85672 /usr/local/bin/adb -L tcp:5037 fork-server server --reply-fd 4
  96918 adb kill-server
  96920 /usr/local/bin/adb -s a728c5d30606 shell getprop
  96986 /usr/local/bin/adb -L tcp:5037 server nodaemon
  12345 /usr/bin/python3 fake_adb.py server
  777 node dist/index.js`
	got := parseADBServers(ps)
	if want := []int{58069, 85672, 96986}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
