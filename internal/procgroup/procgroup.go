// Package procgroup makes a child process die with everything it started.
//
// exec.CommandContext kills only the direct child when its context ends. The
// tools poligon runs are wrappers — `maestro` is a shell script around a JVM,
// a `command` run is `sh -c` around whatever the user wrote — so killing the
// direct child leaves the real worker orphaned, still holding adb forwards and
// the device. Bind puts the child in its own process group and turns the
// context's cancel into SIGTERM to the whole group, then SIGKILL after grace.
package procgroup

import (
	"os/exec"
	"syscall"
	"time"
)

// Grace is how long a group gets between SIGTERM and SIGKILL.
const Grace = 5 * time.Second

// Bind must be called before cmd.Start on a command built with
// exec.CommandContext.
func Bind(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		err := syscall.Kill(-pgid, syscall.SIGTERM)
		time.AfterFunc(Grace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		return err
	}
	// Wait returns once the direct child exits even if a grandchild still
	// holds its stdout pipe open.
	cmd.WaitDelay = Grace + time.Second
}
