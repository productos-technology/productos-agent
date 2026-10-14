//go:build !windows

package update

import (
	"os"
	"syscall"
)

// Replace swaps the staged binary in for exe: a rename over the old path gives it a new inode, so the running process
// keeps the file it was started from and the next exec gets the new one. Never a half-written binary on the path.
func Replace(staged, exe string) error {
	if info, err := os.Stat(exe); err == nil {
		_ = os.Chmod(staged, info.Mode().Perm()|0o111)
	}
	return os.Rename(staged, exe)
}

// Reexec replaces this process with exe, same arguments and environment: the PID stays, so systemd and launchd do
// not notice. It returns only when the exec failed.
func Reexec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
