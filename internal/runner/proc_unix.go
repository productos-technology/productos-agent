//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts the tool and everything it spawns in their own group, so cancel kills all of it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup asks the group politely, then insists. It only touches the pid (Wait owns the rest of cmd).
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	pgid := -pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			_ = syscall.Kill(pgid, syscall.SIGKILL)
			return
		case <-tick.C:
			if syscall.Kill(pgid, 0) != nil {
				return // nobody left in the group
			}
		}
	}
}
