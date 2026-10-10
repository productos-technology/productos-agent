// Package secrets stores the OAuth tokens: in the OS keychain when one is reachable (macOS Keychain, Windows
// Credential Manager, the Secret Service on Linux), else in a 0600 file under the config directory, with a warning.
// Nothing here is ever logged.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/productos-technology/productos-agent/internal/paths"
)

const service = "productos-agent"

// Tokens is what the token endpoint handed out, plus when the access token dies.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
}

// Expiring reports whether the access token dies within the margin.
func (t Tokens) Expiring(margin time.Duration) bool {
	return t.AccessToken == "" || time.Now().Add(margin).After(t.ExpiresAt)
}

// ErrNotFound: nobody is signed in.
var ErrNotFound = errors.New("no stored credentials")

// Store keeps the tokens of one server.
type Store struct {
	account string // the server URL: one credential per server
	// FallbackUsed is set when the keychain was unreachable and the file was used instead.
	FallbackUsed bool
	warned       bool
}

func New(server string) *Store { return &Store{account: server} }

func (s *Store) filePath() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// Save writes the tokens to the keychain, else to the fallback file.
func (s *Store) Save(t Tokens) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if !disabledKeyring() {
		if err := keyring.Set(service, s.account, string(data)); err == nil {
			s.FallbackUsed = false
			return nil
		} else {
			s.warnOnce(err)
		}
	}
	s.FallbackUsed = true
	path, err := s.filePath()
	if err != nil {
		return err
	}
	if err := paths.Ensure(filepath.Dir(path)); err != nil {
		return err
	}
	all, _ := s.readFile()
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	all[s.account] = data
	out, err := json.Marshal(all)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load returns the stored tokens, or ErrNotFound.
func (s *Store) Load() (Tokens, error) {
	var t Tokens
	if !disabledKeyring() {
		raw, err := keyring.Get(service, s.account)
		if err == nil {
			return t, json.Unmarshal([]byte(raw), &t)
		}
		if !errors.Is(err, keyring.ErrNotFound) {
			s.warnOnce(err)
		}
	}
	all, err := s.readFile()
	if err != nil {
		return t, err
	}
	raw, ok := all[s.account]
	if !ok {
		return t, ErrNotFound
	}
	s.FallbackUsed = true
	return t, json.Unmarshal(raw, &t)
}

// Delete forgets the tokens everywhere.
func (s *Store) Delete() error {
	if !disabledKeyring() {
		if err := keyring.Delete(service, s.account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			s.warnOnce(err)
		}
	}
	all, err := s.readFile()
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	delete(all, s.account)
	path, _ := s.filePath()
	if len(all) == 0 {
		return os.Remove(path)
	}
	out, _ := json.Marshal(all)
	return os.WriteFile(path, out, 0o600)
}

func (s *Store) readFile() (map[string]json.RawMessage, error) {
	path, err := s.filePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("credentials file is corrupt: %w", err)
	}
	return all, nil
}

func (s *Store) warnOnce(err error) {
	if s.warned {
		return
	}
	s.warned = true
	fmt.Fprintf(os.Stderr, "warning: the OS keychain is not reachable (%v); credentials are kept in a 0600 file under the config directory instead\n", err)
}

// PRODUCTOS_AGENT_NO_KEYRING=1 skips the keychain (tests, headless servers).
func disabledKeyring() bool { return os.Getenv("PRODUCTOS_AGENT_NO_KEYRING") == "1" }
