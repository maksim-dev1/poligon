package procgroup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A wrapper that backgrounds a worker (like maestro's script around its JVM):
// canceling the context must take the worker down too, not only the wrapper.
func TestCancelKillsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
	Bind(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var worker int
	for range 50 {
		if worker != 0 {
			break
		}
		b, _ := os.ReadFile(pidFile)
		worker, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if worker == 0 {
		t.Fatal("worker never started")
	}

	cancel()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}

	for range 50 {
		if syscall.Kill(worker, 0) != nil {
			return // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(worker, syscall.SIGKILL)
	t.Fatalf("worker %d survived its wrapper", worker)
}
