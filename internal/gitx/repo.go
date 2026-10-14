package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo is a git checkout on disk.
type Repo struct {
	Dir string
}

// Open resolves dir to an absolute path and checks it is a work tree.
func Open(dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Dir: abs}
	out, err := r.git(context.Background(), "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		return nil, fmt.Errorf("%s is not a git checkout", abs)
	}
	top, err := r.git(context.Background(), "rev-parse", "--show-toplevel")
	if err == nil && strings.TrimSpace(top) != "" {
		r.Dir = filepath.Clean(strings.TrimSpace(top))
	}
	return r, nil
}

// GitError carries git's stderr.
type GitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *GitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *GitError) Unwrap() error { return e.Err }

func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &GitError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// Git runs an arbitrary git command (used by the commit and push steps).
func (r *Repo) Git(ctx context.Context, args ...string) (string, error) { return r.git(ctx, args...) }

// RemoteURL is `git remote get-url origin`.
func (r *Repo) RemoteURL(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("the checkout has no `origin` remote")
	}
	return strings.TrimSpace(out), nil
}

// DefaultBranch: origin/HEAD when git knows it, else `git remote show origin`, else main/master if they exist.
func (r *Repo) DefaultBranch(ctx context.Context) (string, error) {
	if out, err := r.git(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(out), "origin/"), nil
	}
	if out, err := r.git(ctx, "remote", "show", "origin"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "HEAD branch:") {
				b := strings.TrimSpace(strings.TrimPrefix(line, "HEAD branch:"))
				if b != "" && b != "(unknown)" {
					return b, nil
				}
			}
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("cannot tell the default branch of origin")
}

// IsClean reports whether there are no staged, unstaged or untracked changes.
func (r *Repo) IsClean(ctx context.Context) (bool, error) {
	out, err := r.git(ctx, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

func (r *Repo) Fetch(ctx context.Context, branch string) error {
	_, err := r.git(ctx, "fetch", "--quiet", "origin", branch)
	return err
}

func (r *Repo) BranchExists(ctx context.Context, name string) bool {
	_, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

func (r *Repo) RemoteBranchExists(ctx context.Context, name string) bool {
	_, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+name)
	return err == nil
}

// CreateBranch checks out a new branch from origin/<from>.
func (r *Repo) CreateBranch(ctx context.Context, name, from string) error {
	_, err := r.git(ctx, "checkout", "--quiet", "-b", name, "origin/"+from)
	return err
}

func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// CommitAll stages everything and commits; returns false when there was nothing to commit.
func (r *Repo) CommitAll(ctx context.Context, message string) (bool, error) {
	return r.commitAll(ctx, message, false)
}

// CommitWIP is CommitAll without the repository's hooks: work in progress is saved even when a pre-commit hook
// would refuse it (the hooks run again on the commit that finishes the work).
func (r *Repo) CommitWIP(ctx context.Context, message string) (bool, error) {
	return r.commitAll(ctx, message, true)
}

func (r *Repo) commitAll(ctx context.Context, message string, noVerify bool) (bool, error) {
	if _, err := r.git(ctx, "add", "-A"); err != nil {
		return false, err
	}
	if _, err := r.git(ctx, "diff", "--cached", "--quiet"); err == nil {
		return false, nil // nothing staged
	}
	args := []string{"-c", "commit.gpgsign=false", "commit", "--quiet", "-m", message}
	if noVerify {
		args = append(args, "--no-verify")
	}
	_, err := r.git(ctx, args...)
	return err == nil, err
}

// Stash saves everything (untracked included) and leaves the tree clean: the last resort when even a WIP commit fails.
func (r *Repo) Stash(ctx context.Context, message string) error {
	_, err := r.git(ctx, "stash", "push", "--include-untracked", "--message", message)
	return err
}

// CommitsWithTrailer lists, one line each, the newest commits on ref whose message contains trailer (fixed string).
func (r *Repo) CommitsWithTrailer(ctx context.Context, ref, trailer string, n int) ([]string, error) {
	out, err := r.git(ctx, "log", "--oneline", "--no-decorate", "-n", strconv.Itoa(n), "--fixed-strings", "--grep="+trailer, ref, "--")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func (r *Repo) Push(ctx context.Context, branch string) error {
	_, err := r.git(ctx, "push", "--quiet", "--set-upstream", "origin", branch)
	return err
}

// Change is one path's state against a base (what the drawer's file_change shows).
type Change struct {
	Path      string
	Action    string // created | modified | deleted | renamed
	From      string
	Additions int
	Deletions int
	Binary    bool
}

// Changes lists the work tree against `base` (a commit), untracked files included (as created, with their line count).
func (r *Repo) Changes(ctx context.Context, base string) ([]Change, error) {
	byPath := map[string]*Change{}
	order := []string{}
	add := func(c Change) {
		if existing, ok := byPath[c.Path]; ok {
			*existing = c
			return
		}
		cp := c
		byPath[c.Path] = &cp
		order = append(order, c.Path)
	}

	status, err := r.git(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	// Status tells actions (and renames); numstat tells line counts.
	actions := map[string]Change{}
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		code, path := e[:2], e[3:]
		c := Change{Path: path, Action: "modified"}
		switch {
		case code == "??" || strings.ContainsRune(code, 'A'):
			c.Action = "created"
		case strings.ContainsRune(code, 'D'):
			c.Action = "deleted"
		case strings.ContainsRune(code, 'R'):
			c.Action = "renamed"
			if i+1 < len(entries) {
				c.From = entries[i+1]
				i++
			}
		}
		actions[path] = c
	}

	numstat, err := r.git(ctx, "diff", "--numstat", "-M", base)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(numstat, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		c := Change{Path: parts[2], Action: "modified"}
		if parts[0] == "-" {
			c.Binary = true
		} else {
			c.Additions, _ = strconv.Atoi(parts[0])
			c.Deletions, _ = strconv.Atoi(parts[1])
		}
		if from, to, ok := splitRename(parts[2]); ok {
			c.Path, c.From, c.Action = to, from, "renamed"
		}
		if a, ok := actions[c.Path]; ok {
			c.Action = a.Action
			if a.From != "" {
				c.From = a.From
			}
		} else if _, err := r.git(ctx, "cat-file", "-e", base+":"+c.Path); err != nil {
			c.Action = "created"
		}
		add(c)
	}
	for path, a := range actions {
		if _, seen := byPath[path]; seen || a.Action != "created" {
			continue
		}
		c := a
		c.Additions = r.lineCount(path)
		add(c)
	}
	out := make([]Change, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	return out, nil
}

func (r *Repo) lineCount(path string) int {
	out, err := exec.Command("git", "-C", r.Dir, "diff", "--no-index", "--numstat", "/dev/null", path).Output()
	if err != nil {
		// diff --no-index exits 1 when files differ; the output is still there.
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return 0
		}
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 3)
	if len(parts) < 1 {
		return 0
	}
	n, _ := strconv.Atoi(parts[0])
	return n
}

// "old => new" or "dir/{old => new}/file" as git prints renames in numstat.
func splitRename(s string) (from, to string, ok bool) {
	if !strings.Contains(s, " => ") {
		return "", "", false
	}
	if open := strings.Index(s, "{"); open >= 0 {
		close := strings.Index(s, "}")
		if close > open {
			prefix, suffix := s[:open], s[close+1:]
			inner := strings.SplitN(s[open+1:close], " => ", 2)
			if len(inner) == 2 {
				return prefix + inner[0] + suffix, prefix + inner[1] + suffix, true
			}
		}
	}
	parts := strings.SplitN(s, " => ", 2)
	return parts[0], parts[1], true
}
