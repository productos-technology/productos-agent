//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
)

// Replace swaps the staged binary in for exe. Windows refuses to overwrite a running executable but lets it be
// renamed: the running one becomes exe.old (removed by CleanOld on the next start) and the new one takes its name.
func Replace(staged, exe string) error {
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Rename(old, exe)
		return err
	}
	return nil
}

// Reexec starts exe with the same arguments and environment plus PRODUCTOS_AGENT_REPLACES=<this pid> (the new process
// then skips the "already running" check for this one), and exits. It returns only when the start failed.
func Reexec(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", ReplacesEnv, os.Getpid()))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
