package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Service labels poligon manages beside itself.
const (
	adbLabel     = "com.pancir.adb"          // LaunchAgent, same gui domain as poligon
	sidecarLabel = "com.pancir.poligon-live" // LaunchDaemon: ws-scrcpy
)

// adbGuard keeps the farm on exactly one healthy adb server.
//
// The server belongs to launchd (com.pancir.adb, deploy/adb-server-run.sh);
// this watches it from poligon's side: a server that stops answering, or a
// second server someone spawned beside it, gets com.pancir.adb restarted. And
// whenever the server changes — restarted here, crashed and respawned by
// launchd, restarted by hand — ws-scrcpy is restarted too: its adbkit tracker
// keeps talking to the dead server's socket and never shows a phone again.
type adbGuard struct {
	adb  string
	addr string // server's host:port
	uid  int
	log  *slog.Logger

	lastPID  int         // server pid seen on the previous check
	wedged   int         // consecutive checks whose probe timed out
	restarts []time.Time // our own restarts of com.pancir.adb, for rate limiting
}

func newADBGuard(adbPath, addr string, log *slog.Logger) *adbGuard {
	if adbPath == "" {
		adbPath = "adb"
	}
	if addr == "" {
		addr = "127.0.0.1:5037"
	}
	return &adbGuard{adb: adbPath, addr: addr, uid: os.Getuid(), log: log}
}

// check runs once per watchdog tick and returns the number of attached
// devices, -1 when adb could not answer.
func (g *adbGuard) check(ctx context.Context) int {
	servers := adbServerPIDs()
	managed := g.managed()

	// Asking `adb` anything while no server listens makes the client spawn a
	// daemonized server of its own — exactly the stray we are avoiding.
	if !listening(g.addr) {
		if managed {
			g.log.Warn("adb: server not listening — launchd is (re)starting it", "servers", len(servers))
		} else {
			g.log.Warn("adb: server not listening and " + adbLabel + " is not installed — run scripts/install-all.sh")
		}
		return -1
	}

	n := g.probe(ctx)
	if n < 0 {
		g.wedged++
	} else {
		g.wedged = 0
	}

	var why string
	switch {
	case len(servers) > 1:
		why = fmt.Sprintf("%d adb servers running (pids %v) — only one may hold the phones' USB", len(servers), servers)
	case g.wedged >= 2:
		why = "adb server stopped answering"
	}
	if why != "" {
		g.restartADB(why, managed)
	}

	if len(servers) == 1 {
		pid := servers[0]
		if g.lastPID != 0 && pid != g.lastPID {
			g.log.Warn("adb: server changed — restarting ws-scrcpy so the live screens reconnect",
				"old_pid", g.lastPID, "new_pid", pid)
			g.restartSidecar()
		}
		g.lastPID = pid
	}
	return n
}

// probe lists devices with a timeout; -1 when the server does not answer.
func (g *adbGuard) probe(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, g.adb, "devices").Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, ln := range strings.Split(string(out), "\n")[1:] {
		if strings.HasSuffix(strings.TrimSpace(ln), "\tdevice") {
			n++
		}
	}
	return n
}

func (g *adbGuard) restartADB(why string, managed bool) {
	if !managed {
		g.log.Error("adb: "+why+"; "+adbLabel+" is not installed so poligon cannot restart it — run scripts/install-all.sh",
			"manual_fix", "pkill -9 -x adb")
		return
	}
	// at most 3 restarts per 30 minutes: if a restart does not help, looping
	// only keeps dropping every live Android screen
	cut := time.Now().Add(-30 * time.Minute)
	kept := g.restarts[:0]
	for _, t := range g.restarts {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	g.restarts = kept
	if len(g.restarts) >= 3 {
		g.log.Error("adb: " + why + "; already restarted 3× in 30 min — leaving it for a human (scripts/farm-doctor.sh)")
		return
	}
	g.restarts = append(g.restarts, time.Now())
	g.wedged = 0
	g.log.Warn("adb: "+why+" — restarting "+adbLabel, "attempt", len(g.restarts))
	target := fmt.Sprintf("gui/%d/%s", g.uid, adbLabel)
	if out, err := launchctl("kickstart", "-k", target); err != nil {
		g.log.Error("adb: restart failed", "err", err, "out", out)
	}
	// the new server's pid shows up on the next check, which then restarts
	// ws-scrcpy
}

// restartSidecar restarts ws-scrcpy. It is a system LaunchDaemon, so this
// needs the NOPASSWD sudoers rule scripts/install-all.sh installs.
func (g *adbGuard) restartSidecar() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", "-n", "/bin/launchctl", "kickstart", "-k", "system/"+sidecarLabel).CombinedOutput()
	if err != nil {
		g.log.Error("adb: could not restart ws-scrcpy — Android live screens stay dark until it is",
			"err", err, "out", strings.TrimSpace(string(out)),
			"manual_fix", "sudo launchctl kickstart -k system/"+sidecarLabel,
			"permanent_fix", "scripts/install-all.sh (installs the sudoers rule)")
	}
}

func (g *adbGuard) managed() bool {
	_, err := launchctl("print", fmt.Sprintf("gui/%d/%s", g.uid, adbLabel))
	return err == nil
}

func launchctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// adbServerPIDs lists running adb *server* processes — `adb … server …` or
// `adb … fork-server server …`, not clients like `adb devices` or
// `adb kill-server`.
func adbServerPIDs() []int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	return parseADBServers(string(out))
}

func parseADBServers(ps string) []int {
	var pids []int
	for _, ln := range strings.Split(ps, "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 || filepath.Base(f[1]) != "adb" {
			continue
		}
		for _, a := range f[2:] {
			if a == "server" {
				if pid, err := strconv.Atoi(f[0]); err == nil {
					pids = append(pids, pid)
				}
				break
			}
		}
	}
	return pids
}
