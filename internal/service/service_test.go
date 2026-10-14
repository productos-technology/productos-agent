package service

import (
	"strings"
	"testing"
)

func TestEnvironmentCarriesTheToolConfigDirs(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/pat/.claude-work")
	t.Setenv("CODEX_HOME", "")
	got := environment()
	want := [][2]string{{"PATH", "/usr/bin"}, {"CLAUDE_CONFIG_DIR", "/home/pat/.claude-work"}}
	for _, kv := range got {
		if kv[0] == "CODEX_HOME" {
			t.Fatalf("an empty variable must not be carried: %v", got)
		}
	}
	if len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("environment() = %v, want it to start with %v", got, want)
	}
}

func TestPlistEnvironmentEscapes(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("CLAUDE_CONFIG_DIR", `/Users/pat/a&b`)
	out := plistEnvironment()
	if !strings.Contains(out, "<key>CLAUDE_CONFIG_DIR</key><string>/Users/pat/a&amp;b</string>") {
		t.Fatalf("plistEnvironment() = %q", out)
	}
}

func TestStablePathForHomebrew(t *testing.T) {
	exists := func(p string) bool { return strings.Contains(p, "/opt/productos-agent/") }
	cases := map[string]string{
		"/opt/homebrew/Cellar/productos-agent/0.1.0/bin/productos-agent":              "/opt/homebrew/opt/productos-agent/bin/productos-agent",
		"/usr/local/Cellar/productos-agent/0.1.0_1/bin/productos-agent":               "/usr/local/opt/productos-agent/bin/productos-agent",
		"/home/linuxbrew/.linuxbrew/Cellar/productos-agent/0.2.0/bin/productos-agent": "/home/linuxbrew/.linuxbrew/opt/productos-agent/bin/productos-agent",
		"/usr/local/bin/productos-agent":                                              "/usr/local/bin/productos-agent",
		"/home/pat/.local/bin/productos-agent":                                        "/home/pat/.local/bin/productos-agent",
	}
	for in, want := range cases {
		if got := stablePath(in, exists); got != want {
			t.Errorf("stablePath(%q) = %q, want %q", in, got, want)
		}
	}
	// Without the opt link (an unusual prefix), keep the real path rather than a broken one.
	if got := stablePath("/opt/homebrew/Cellar/productos-agent/0.1.0/bin/productos-agent", func(string) bool { return false }); got != "/opt/homebrew/Cellar/productos-agent/0.1.0/bin/productos-agent" {
		t.Errorf("got %q", got)
	}
}

func TestServiceBinaryIsReadBack(t *testing.T) {
	unit := "[Service]\nExecStart=/home/pat/.local/bin/productos-agent run\nRestart=always\n"
	if got := unitBinary(unit); got != "/home/pat/.local/bin/productos-agent" {
		t.Errorf("unitBinary = %q", got)
	}
	plist := "<key>ProgramArguments</key>\n  <array><string>/opt/homebrew/opt/productos-agent/bin/productos-agent</string><string>run</string></array>"
	if got := plistBinary(plist); got != "/opt/homebrew/opt/productos-agent/bin/productos-agent" {
		t.Errorf("plistBinary = %q", got)
	}
	if unitBinary("[Service]\n") != "" || plistBinary("<dict/>") != "" {
		t.Error("nothing to read back is empty")
	}
}
