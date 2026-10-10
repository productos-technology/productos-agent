// Package service installs the agent as a per-user background service: a systemd user unit on Linux, a launchd
// agent on macOS, a logon task in the Task Scheduler on Windows. Each does the simplest reliable thing.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	name     = "productos-agent"
	label    = "technology.productos.agent"
	taskName = "ProductOS Agent"
)

// Manager is the per-OS implementation.
type Manager interface {
	Install(binary string) (string, error)
	Uninstall() error
	Start() error
	Stop() error
	// Describe explains what Install writes (shown to the person).
	Describe() string
}

// New picks the manager of the current OS.
func New() (Manager, error) {
	switch runtime.GOOS {
	case "linux":
		return systemd{}, nil
	case "darwin":
		return launchd{}, nil
	case "windows":
		return schtasks{}, nil
	}
	return nil, fmt.Errorf("no service manager for %s", runtime.GOOS)
}

// Binary resolves the running executable's absolute path (what the unit runs).
func Binary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func run(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", cmd, strings.Join(args, " "), msg)
	}
	return nil
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// --- Linux: systemd --user ---

type systemd struct{}

func (systemd) unitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home(), ".config")
	}
	return filepath.Join(base, "systemd", "user", name+".service")
}

func (s systemd) Describe() string {
	return "systemd user unit " + s.unitPath() + " (enable lingering with `loginctl enable-linger $USER` so it runs while you are logged out)"
}

func (s systemd) Install(binary string) (string, error) {
	unit := fmt.Sprintf(`[Unit]
Description=ProductOS Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run
Restart=always
RestartSec=10
Environment=PATH=%s

[Install]
WantedBy=default.target
`, binary, os.Getenv("PATH"))
	path := s.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return path, err
	}
	return path, run("systemctl", "--user", "enable", "--now", name+".service")
}

func (s systemd) Uninstall() error {
	_ = run("systemctl", "--user", "disable", "--now", name+".service")
	if err := os.Remove(s.unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return run("systemctl", "--user", "daemon-reload")
}

func (systemd) Start() error { return run("systemctl", "--user", "start", name+".service") }
func (systemd) Stop() error  { return run("systemctl", "--user", "stop", name+".service") }

// --- macOS: launchd agent ---

type launchd struct{}

func (launchd) plistPath() string {
	return filepath.Join(home(), "Library", "LaunchAgents", label+".plist")
}

func (l launchd) Describe() string { return "launchd agent " + l.plistPath() }

func (l launchd) Install(binary string) (string, error) {
	logDir := filepath.Join(home(), "Library", "Logs", name)
	_ = os.MkdirAll(logDir, 0o755)
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>%s</string></dict>
  <key>StandardOutPath</key><string>%s/launchd.out.log</string>
  <key>StandardErrorPath</key><string>%s/launchd.err.log</string>
</dict>
</plist>
`, label, binary, os.Getenv("PATH"), logDir, logDir)
	path := l.plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	_ = l.Stop()
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return "", err
	}
	return path, l.Start()
}

func (l launchd) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (l launchd) Uninstall() error {
	_ = l.Stop()
	if err := os.Remove(l.plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (l launchd) Start() error {
	if err := run("launchctl", "bootstrap", l.domain(), l.plistPath()); err != nil {
		// Older macOS: fall back to load.
		return run("launchctl", "load", "-w", l.plistPath())
	}
	return nil
}

func (l launchd) Stop() error {
	if err := run("launchctl", "bootout", l.domain()+"/"+label); err != nil {
		return run("launchctl", "unload", l.plistPath())
	}
	return nil
}

// --- Windows: Task Scheduler, at logon ---

type schtasks struct{}

func (schtasks) Describe() string {
	return "Task Scheduler task \"" + taskName + "\" (runs at logon, restarts if it stops)"
}

func (s schtasks) Install(binary string) (string, error) {
	_ = s.Uninstall()
	cmd := fmt.Sprintf(`"%s" run`, binary)
	if err := run("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", taskName, "/TR", cmd); err != nil {
		return "", err
	}
	return taskName, s.Start()
}

func (schtasks) Uninstall() error {
	_ = run("schtasks", "/End", "/TN", taskName)
	return run("schtasks", "/Delete", "/F", "/TN", taskName)
}

func (schtasks) Start() error { return run("schtasks", "/Run", "/TN", taskName) }
func (schtasks) Stop() error  { return run("schtasks", "/End", "/TN", taskName) }
