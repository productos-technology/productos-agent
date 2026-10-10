// Package tools detects the coding tools the agent can drive: `claude` (Claude Code) and `codex` (OpenAI Codex), on
// PATH or in the usual install directories, with their versions.
package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/protocol"
)

const (
	Claude = "claude"
	Codex  = "codex"
)

var Known = []string{Claude, Codex}

var versionRE = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.-]+)?`)

// Detect looks every known tool up and asks it for its version (3 s each).
func Detect(ctx context.Context) protocol.Tools {
	out := protocol.Tools{}
	for _, name := range Known {
		out[name] = detectOne(ctx, name)
	}
	return out
}

// Path returns where a tool lives: the env override (PRODUCTOS_AGENT_CLAUDE_BIN / PRODUCTOS_AGENT_CODEX_BIN), PATH,
// then the common install dirs.
func Path(name string) (string, bool) {
	if override := os.Getenv("PRODUCTOS_AGENT_" + strings.ToUpper(name) + "_BIN"); override != "" {
		return override, true
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	for _, dir := range candidateDirs() {
		for _, candidate := range []string{filepath.Join(dir, name), filepath.Join(dir, name+".exe"), filepath.Join(dir, name+".cmd")} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, true
			}
		}
	}
	return "", false
}

func detectOne(ctx context.Context, name string) protocol.ToolInfo {
	path, ok := Path(name)
	if !ok {
		return protocol.ToolInfo{}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	info := protocol.ToolInfo{Available: true, Path: path}
	if err == nil {
		if m := versionRE.FindString(string(raw)); m != "" {
			info.Version = m
		}
	}
	return info
}

func candidateDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".claude", "local"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, "bin"),
		"/usr/local/bin",
		"/opt/homebrew/bin",
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			dirs = append(dirs, filepath.Join(appData, "npm"))
		}
		if local := os.Getenv("LocalAppData"); local != "" {
			dirs = append(dirs, filepath.Join(local, "Programs", "claude"))
		}
	}
	return dirs
}

// Changed reports whether two detections differ in availability or version.
func Changed(a, b protocol.Tools) bool {
	for _, name := range Known {
		if a[name].Available != b[name].Available || a[name].Version != b[name].Version {
			return true
		}
	}
	return false
}
