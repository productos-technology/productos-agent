// Package paths resolves where the agent keeps its configuration, state and logs on each platform.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

const appDir = "productos-agent"

// ConfigDir is where config.json (and the credentials fallback file) live.
//
//	linux    $XDG_CONFIG_HOME/productos-agent  (~/.config/productos-agent)
//	darwin   ~/Library/Application Support/productos-agent
//	windows  %AppData%\productos-agent
func ConfigDir() (string, error) {
	if override := os.Getenv("PRODUCTOS_AGENT_CONFIG_DIR"); override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDir), nil
}

// StateDir is where logs and run scratch files live.
//
//	linux    $XDG_STATE_HOME/productos-agent  (~/.local/state/productos-agent)
//	darwin   ~/Library/Application Support/productos-agent
//	windows  %LocalAppData%\productos-agent
func StateDir() (string, error) {
	if override := os.Getenv("PRODUCTOS_AGENT_STATE_DIR"); override != "" {
		return override, nil
	}
	switch runtime.GOOS {
	case "linux":
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, appDir), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", appDir), nil
	case "windows":
		if local := os.Getenv("LocalAppData"); local != "" {
			return filepath.Join(local, appDir), nil
		}
	}
	return ConfigDir()
}

// LogFile is the structured log of the daemon.
func LogFile() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent.log"), nil
}

// Ensure creates dir with owner-only permissions.
func Ensure(dir string) error { return os.MkdirAll(dir, 0o700) }
