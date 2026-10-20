// Package worktree keeps the git worktrees the agent runs changes in: one per change, under
// <StateDir>/worktrees/<repository_id>/<branch>, never the person's own checkout. A worktree outlives its run, so the
// next round of the same change ("Adjust") continues where the last one stopped; `productos-agent worktrees` lists
// them and `prune` removes the ones whose branch is merged or that nobody used for 14 days. The Coordinator keeps the
// device's concurrent runs from racing on one repository (fetch, worktree add/remove, push) or sharing a directory.
package worktree

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/productos-technology/productos-agent/internal/gitx"
	"github.com/productos-technology/productos-agent/internal/paths"
)

// UnusedAfter is how long a worktree may sit unused before prune removes it.
const UnusedAfter = 14 * 24 * time.Hour

// Root is <StateDir>/worktrees.
func Root() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worktrees"), nil
}

// PathFor is where the worktree of a branch of a repository lives: root/<repository_id>/<branch, made safe>.
func PathFor(root, repositoryID, fullName, branch string) string {
	repo := gitx.Slug(repositoryID, 64)
	if repo == "" {
		repo = gitx.Slug(fullName, 64)
	}
	if repo == "" {
		repo = "repository"
	}
	name := gitx.Slug(branch, 120)
	if name == "" {
		name = "branch"
	}
	return filepath.Join(root, repo, name)
}

// Ensure creates the directories above a worktree path, owner-only.
func Ensure(path string) error { return os.MkdirAll(filepath.Dir(path), 0o700) }

// Inside reports whether path is strictly under root (nothing outside the agent's own directory is ever removed).
func Inside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !startsWithDotDot(rel)
}

func startsWithDotDot(rel string) bool {
	return len(rel) >= 3 && rel[:2] == ".." && os.IsPathSeparator(rel[2])
}

// Meta is the sidecar `<path>.json` the agent keeps beside each worktree: what it is for and when a run last used it.
type Meta struct {
	Path          string    `json:"path"`
	Branch        string    `json:"branch"`
	RepositoryID  string    `json:"repository_id,omitempty"`
	Repository    string    `json:"repository,omitempty"`
	Checkout      string    `json:"checkout"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    time.Time `json:"last_used_at"`
	LastRunID     string    `json:"last_run_id,omitempty"`
}

func metaFile(path string) string { return filepath.Clean(path) + ".json" }

// ReadMeta reads the sidecar of a worktree path (nil, nil when there is none).
func ReadMeta(path string) (*Meta, error) {
	data, err := os.ReadFile(metaFile(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Path == "" {
		m.Path = filepath.Clean(path)
	}
	return &m, nil
}

// WriteMeta writes the sidecar atomically (0600), keeping the first CreatedAt.
func WriteMeta(m Meta) error {
	if m.Path == "" {
		return errors.New("worktree meta without a path")
	}
	if old, err := ReadMeta(m.Path); err == nil && old != nil && !old.CreatedAt.IsZero() {
		m.CreatedAt = old.CreatedAt
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if err := Ensure(m.Path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := metaFile(m.Path) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, metaFile(m.Path))
}

// Coordinator serialises the git work of the device's concurrent runs on one repository (fetch, worktree add and
// remove, push: they share one .git) and keeps two runs out of one directory. The zero value is not usable: New.
type Coordinator struct {
	mu      sync.Mutex
	repos   map[string]*sync.Mutex
	claimed map[string]string
}

func NewCoordinator() *Coordinator {
	return &Coordinator{repos: map[string]*sync.Mutex{}, claimed: map[string]string{}}
}

// LockRepo takes the repository's mutex (key: the person's checkout, which owns the .git every worktree shares) and
// returns the unlock.
func (c *Coordinator) LockRepo(key string) func() {
	key = canonicalKey(key)
	c.mu.Lock()
	m := c.repos[key]
	if m == nil {
		m = &sync.Mutex{}
		c.repos[key] = m
	}
	c.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Claim marks dir as the working directory of owner (a run id) until release; false with the holder when another
// run has it.
func (c *Coordinator) Claim(dir, owner string) (release func(), holder string, ok bool) {
	key := canonicalKey(dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	if h, taken := c.claimed[key]; taken && h != owner {
		return nil, h, false
	}
	c.claimed[key] = owner
	return func() {
		c.mu.Lock()
		if c.claimed[key] == owner {
			delete(c.claimed, key)
		}
		c.mu.Unlock()
	}, owner, true
}

// Claimed reports whether a run works in dir now.
func (c *Coordinator) Claimed(dir string) bool {
	key := canonicalKey(dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.claimed[key]
	return ok
}

// ClaimedDirs lists the directories runs work in now, sorted (the daemon publishes them for the CLI's prune).
func (c *Coordinator) ClaimedDirs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.claimed))
	for dir := range c.claimed {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

func canonicalKey(dir string) string { return gitx.Canonical(dir) }
