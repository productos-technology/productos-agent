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

// killProcessGroup asks politely, then insists.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
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
			if cmd.ProcessState != nil || syscall.Kill(pgid, 0) != nil {
				return
			}
		}
	}
}
