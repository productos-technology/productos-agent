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
