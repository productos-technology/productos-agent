// Package service installs the agent as a per-user background service: a systemd user unit on Linux, a launchd
// agent on macOS, a logon task in the Task Scheduler on Windows. Each does the simplest reliable thing.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Restart stops and starts the installed service (after a manual update).
	Restart() error
	// Installed reports whether the service is installed.
	Installed() bool
	// Binary is the executable the installed service runs ("" when unknown): an update restarts the service only
	// when it replaced that file.
	Binary() string
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

// Binary resolves the running executable's absolute path (what the unit runs). A Homebrew keg lives under a versioned
// Cellar path that `brew upgrade` deletes, so the unit gets Homebrew's stable opt link instead.
func Binary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return stablePath(resolved, func(p string) bool { _, err := os.Stat(p); return err == nil }), nil
}

// stablePath maps <prefix>/Cellar/productos-agent/<version>/bin/productos-agent to
// <prefix>/opt/productos-agent/bin/productos-agent (/opt/homebrew, /usr/local, Linuxbrew) when that link exists.
func stablePath(resolved string, exists func(string) bool) string {
	i := strings.Index(resolved, "/Cellar/"+name+"/")
	if i < 0 {
		return resolved
	}
	rest := resolved[i+len("/Cellar/"+name+"/"):] // <version>/bin/productos-agent
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return resolved
	}
	opt := resolved[:i] + "/opt/" + name + "/" + rest[slash+1:]
	if exists(opt) {
		return opt
	}
	return resolved
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

// inheritedVars are the variables the service carries over from the shell that installs it, besides PATH: where
// Claude Code and Codex keep their sign-in (CLAUDE_CONFIG_DIR, CODEX_HOME), the XDG base directories and the agent's
// own directories. A service starts with none of the shell's environment, so without these a run would use the
// tool's default config directory, and a person signed in elsewhere (CLAUDE_CONFIG_DIR=~/.claude-work) would see
// "OAuth session expired" from a stale default. Never a secret: the unit file is world-readable.
var inheritedVars = []string{
	"CLAUDE_CONFIG_DIR", "CODEX_HOME",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME",
	"PRODUCTOS_AGENT_CONFIG_DIR", "PRODUCTOS_AGENT_STATE_DIR", "PRODUCTOS_AGENT_NO_KEYRING",
}

// environment returns PATH and the inherited variables that are set, in order, as name=value pairs.
func environment() [][2]string {
	vars := [][2]string{{"PATH", os.Getenv("PATH")}}
	for _, k := range inheritedVars {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			vars = append(vars, [2]string{k, v})
		}
	}
	return vars
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
	var env strings.Builder
	for _, kv := range environment() {
		// systemd quoting: double quotes, with backslashes and double quotes escaped.
		fmt.Fprintf(&env, "Environment=%s=\"%s\"\n", kv[0], strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(kv[1]))
	}
	unit := fmt.Sprintf(`[Unit]
Description=ProductOS Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run
Restart=always
RestartSec=10
%s
[Install]
WantedBy=default.target
`, binary, env.String())
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

func (systemd) Start() error   { return run("systemctl", "--user", "start", name+".service") }
func (systemd) Stop() error    { return run("systemctl", "--user", "stop", name+".service") }
func (systemd) Restart() error { return run("systemctl", "--user", "restart", name+".service") }
func (s systemd) Installed() bool {
	_, err := os.Stat(s.unitPath())
	return err == nil
}

func (s systemd) Binary() string {
	data, err := os.ReadFile(s.unitPath())
	if err != nil {
		return ""
	}
	return unitBinary(string(data))
}

// unitBinary reads `ExecStart=<binary> run` back from a unit Install wrote.
func unitBinary(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			return strings.TrimSuffix(rest, " run")
		}
	}
	return ""
}

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
  <dict>%s</dict>
  <key>StandardOutPath</key><string>%s/launchd.out.log</string>
  <key>StandardErrorPath</key><string>%s/launchd.err.log</string>
</dict>
</plist>
`, label, binary, plistEnvironment(), logDir, logDir)
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

// plistEnvironment renders the inherited environment as plist dict entries, XML-escaped.
func plistEnvironment() string {
	var b strings.Builder
	for _, kv := range environment() {
		fmt.Fprintf(&b, "\n    <key>%s</key><string>%s</string>", kv[0], xmlEscape(kv[1]))
	}
	b.WriteString("\n  ")
	return b.String()
}

func xmlEscape(v string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(v)
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

// Restart: kickstart -k kills the running agent and starts it again from the plist.
func (l launchd) Restart() error {
	if err := run("launchctl", "kickstart", "-k", l.domain()+"/"+label); err != nil {
		_ = l.Stop()
		return l.Start()
	}
	return nil
}

func (l launchd) Installed() bool {
	_, err := os.Stat(l.plistPath())
	return err == nil
}

func (l launchd) Binary() string {
	data, err := os.ReadFile(l.plistPath())
	if err != nil {
		return ""
	}
	return plistBinary(string(data))
}

var programArguments = regexp.MustCompile(`<key>ProgramArguments</key>\s*<array>\s*<string>([^<]*)</string>`)

// plistBinary reads the first ProgramArguments string back from a plist Install wrote.
func plistBinary(plist string) string {
	m := programArguments.FindStringSubmatch(plist)
	if m == nil {
		return ""
	}
	return strings.NewReplacer("&quot;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(m[1])
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
func (s schtasks) Restart() error {
	_ = s.Stop()
	return s.Start()
}
func (schtasks) Installed() bool { return run("schtasks", "/Query", "/TN", taskName) == nil }

// Binary is unknown for the scheduled task (reading it back means parsing its XML): an update does not restart it.
func (schtasks) Binary() string { return "" }
