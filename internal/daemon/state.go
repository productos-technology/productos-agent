package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/productos-technology/productos-agent/internal/paths"
	"github.com/productos-technology/productos-agent/internal/protocol"
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
	// Version, InstallMethod and Agent (the server's version block, when it sent one) feed `status`, `doctor` and
	// `update`.
	Version       string                  `json:"version,omitempty"`
	InstallMethod string                  `json:"install_method,omitempty"`
	Agent         *protocol.AgentVersions `json:"agent,omitempty"`
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

// Handoff is what an agent that updates itself before a run leaves for the process that replaces it: the run.start
// messages it acknowledged but did not start. The new process reads it before connecting, deletes it, and resumes
// those runs from the checkout step.
type Handoff struct {
	From      string                   `json:"from_version"`
	To        string                   `json:"to_version"`
	PID       int                      `json:"pid"`
	WrittenAt time.Time                `json:"written_at"`
	Runs      []protocol.ServerMessage `json:"runs"`
	// Features is what the old process's server announced, until the new process hears its own hello.
	Features []string `json:"features,omitempty"`
}

// handoffMaxAge: an older handoff belongs to an update that did not come back in time; its runs fail as
// agent_restarted instead of starting late.
const handoffMaxAge = 10 * time.Minute

func handoffFile() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "handoff.json"), nil
}

// writeHandoff writes handoff.json (0600) atomically.
func writeHandoff(h Handoff) error {
	path, err := handoffFile()
	if err != nil {
		return err
	}
	if err := paths.Ensure(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// takeHandoff reads and deletes handoff.json; nil when there is none.
func takeHandoff() (*Handoff, error) {
	path, err := handoffFile()
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
	_ = os.Remove(path)
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func removeHandoff() {
	if path, err := handoffFile(); err == nil {
		_ = os.Remove(path)
	}
}
