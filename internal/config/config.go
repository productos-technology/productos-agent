// Package config is the agent's small on-disk configuration (never a secret): the server, the OAuth client it
// registered there, the device it registered, and local preferences.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/productos-technology/productos-agent/internal/paths"
)

const DefaultServer = "https://productos.technology"

// Config is config.json. Secrets live in the secrets package.
type Config struct {
	Server     string `json:"server"`
	ClientID   string `json:"client_id,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	// Push the branch when a run ends (default true). PR creation follows the server's options.open_pr.
	Push *bool `json:"push,omitempty"`
	// Extra arguments appended to the tool command lines, for people who need a model or a flag.
	ClaudeArgs []string `json:"claude_args,omitempty"`
	CodexArgs  []string `json:"codex_args,omitempty"`
	// ClaudePermissionMode overrides the default "acceptEdits"; CodexSandbox the default "workspace-write".
	ClaudePermissionMode string `json:"claude_permission_mode,omitempty"`
	CodexSandbox         string `json:"codex_sandbox,omitempty"`
	// AutoUpdate lets the daemon update itself to the server's latest release before a run or when idle (default
	// true). PRODUCTOS_AGENT_NO_UPDATE=1 turns it off too.
	AutoUpdate *bool `json:"auto_update,omitempty"`
	// Worktrees lets a run the server asks to isolate (options.worktree) work in its own git worktree under the state
	// directory (default true). false keeps every run in the linked checkout, one at a time.
	Worktrees *bool `json:"worktrees,omitempty"`
	// MaxParallelRuns is how many runs this device takes at once (default 3, at least 1), announced to the server.
	MaxParallelRuns int `json:"max_parallel_runs,omitempty"`
}

// DefaultParallelRuns is max_parallel_runs when config.json says nothing.
const DefaultParallelRuns = 3

func (c *Config) PushEnabled() bool { return c.Push == nil || *c.Push }

// WorktreesEnabled: on unless config.json says "worktrees": false.
func (c *Config) WorktreesEnabled() bool { return c.Worktrees == nil || *c.Worktrees }

// ParallelRuns is how many runs the daemon accepts at once. Without worktrees every run happens in the linked
// checkout, so it is 1 whatever max_parallel_runs says (the server's one-run-per-checkout rule).
func (c *Config) ParallelRuns() int {
	if !c.WorktreesEnabled() {
		return 1
	}
	if c.MaxParallelRuns <= 0 {
		return DefaultParallelRuns
	}
	return max(c.MaxParallelRuns, 1)
}

// AutoUpdateEnabled: on unless config.json or the environment turns it off.
func (c *Config) AutoUpdateEnabled() bool {
	if os.Getenv("PRODUCTOS_AGENT_NO_UPDATE") == "1" {
		return false
	}
	return c.AutoUpdate == nil || *c.AutoUpdate
}

func file() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads config.json; a missing file is an empty config with the default server.
func Load() (*Config, error) {
	path, err := file()
	if err != nil {
		return nil, err
	}
	cfg := &Config{Server: DefaultServer}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Server == "" {
		cfg.Server = DefaultServer
	}
	return cfg, nil
}

// Save writes config.json atomically with owner-only permissions.
func Save(cfg *Config) error {
	path, err := file()
	if err != nil {
		return err
	}
	if err := paths.Ensure(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
