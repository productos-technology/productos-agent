// Package config is the agent's small on-disk configuration (never a secret): the server, the OAuth client it
// registered there, the device it registered, and local preferences.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Hamdan85/productos-agent/internal/paths"
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
	// ClaudePermissionMode overrides the default "acceptEdits".
	ClaudePermissionMode string `json:"claude_permission_mode,omitempty"`
}

func (c *Config) PushEnabled() bool { return c.Push == nil || *c.Push }

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
