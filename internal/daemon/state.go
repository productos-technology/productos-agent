package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/productos/productos-agent/internal/paths"
)

// State is what the daemon publishes for `status`: a small JSON file in the state directory.
type State struct {
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	Connected     bool      `json:"connected"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	ActiveRuns    []string  `json:"active_runs"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func stateFile() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.json"), nil
}

func writeState(s State) {
	path, err := stateFile()
	if err != nil {
		return
	}
	if err := paths.Ensure(filepath.Dir(path)); err != nil {
		return
	}
	s.UpdatedAt = time.Now()
	data, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(path, data, 0o600)
}

// ReadState returns the last published state, or nil when there is none.
func ReadState() (*State, error) {
	path, err := stateFile()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Alive reports whether the state's process still exists.
func (s *State) Alive() bool {
	if s == nil || s.PID <= 0 {
		return false
	}
	return processAlive(s.PID)
}

func clearState() {
	if path, err := stateFile(); err == nil {
		_ = os.Remove(path)
	}
}
