package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Worktree is one entry of `git worktree list --porcelain`: the main checkout first, then the linked worktrees.
type Worktree struct {
	Path string
	Head string
	// Branch is the short name of the checked-out branch ("" when detached or bare).
	Branch   string
	Bare     bool
	Detached bool
	Locked   bool
	// Prunable: git knows the worktree but its directory is gone (`git worktree prune` drops it).
	Prunable bool
}

// WorktreeList lists every worktree of the repository, the main checkout first.
func (r *Repo) WorktreeList(ctx context.Context) ([]Worktree, error) {
	out, err := r.git(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

func parseWorktrees(out string) []Worktree {
	var list []Worktree
	var cur *Worktree
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			cur = nil
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		if key == "worktree" {
			list = append(list, Worktree{Path: filepath.Clean(value)})
			cur = &list[len(list)-1]
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.Head = value
		case "branch":
			cur.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "bare":
			cur.Bare = true
		case "detached":
			cur.Detached = true
		case "locked":
			cur.Locked = true
		case "prunable":
			cur.Prunable = true
		}
	}
	return list
}

// WorktreeAdd adds a worktree at path on an existing local branch.
func (r *Repo) WorktreeAdd(ctx context.Context, path, branch string) error {
	_, err := r.git(ctx, "worktree", "add", "--quiet", path, branch)
	return err
}

// WorktreeAddNew adds a worktree at path on a new branch started at from (a commit-ish such as origin/main). The
// branch tracks nothing until the first push sets its upstream.
func (r *Repo) WorktreeAddNew(ctx context.Context, path, branch, from string) error {
	_, err := r.git(ctx, "worktree", "add", "--quiet", "--no-track", "-b", branch, path, from)
	return err
}

// WorktreeRemove removes a linked worktree; git refuses an unclean one unless force (ignored files do not count).
func (r *Repo) WorktreeRemove(ctx context.Context, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	_, err := r.git(ctx, append(args, path)...)
	return err
}

// WorktreePrune drops the registrations of worktrees whose directory is gone.
func (r *Repo) WorktreePrune(ctx context.Context) error {
	_, err := r.git(ctx, "worktree", "prune")
	return err
}

// IsBranchCheckedOut reports whether branch is checked out in any worktree of the repository (the main checkout
// included), and where.
func (r *Repo) IsBranchCheckedOut(ctx context.Context, branch string) (string, bool, error) {
	list, err := r.WorktreeList(ctx)
	if err != nil {
		return "", false, err
	}
	for _, w := range list {
		if w.Branch == branch && !w.Prunable {
			return w.Path, true, nil
		}
	}
	return "", false, nil
}

// MainCheckout is the repository's main worktree (the person's own checkout), whichever worktree r is.
func (r *Repo) MainCheckout(ctx context.Context) (string, error) {
	list, err := r.WorktreeList(ctx)
	if err != nil {
		return "", err
	}
	if len(list) == 0 || list[0].Bare {
		return "", errors.New("the repository has no main checkout")
	}
	return list[0].Path, nil
}

// Relation is how a local branch stands against its remote counterpart.
type Relation int

const (
	Same Relation = iota
	// Behind: the local branch is an ancestor of the remote one (a fast-forward catches up).
	Behind
	// Ahead: the remote branch is an ancestor of the local one (a push sends the rest).
	Ahead
	// Diverged: each has commits the other lacks.
	Diverged
)

func (rel Relation) String() string {
	return [...]string{"same", "behind", "ahead", "diverged"}[rel]
}

// Compare tells how local stands against remote (two commit-ishes, e.g. a branch and origin/<branch>).
func (r *Repo) Compare(ctx context.Context, local, remote string) (Relation, error) {
	a, err := r.revParse(ctx, local)
	if err != nil {
		return Same, err
	}
	b, err := r.revParse(ctx, remote)
	if err != nil {
		return Same, err
	}
	if a == b {
		return Same, nil
	}
	if behind, err := r.IsAncestor(ctx, a, b); err != nil {
		return Same, err
	} else if behind {
		return Behind, nil
	}
	if ahead, err := r.IsAncestor(ctx, b, a); err != nil {
		return Same, err
	} else if ahead {
		return Ahead, nil
	}
	return Diverged, nil
}

// Diverged reports whether local and remote each have commits the other lacks.
func (r *Repo) Diverged(ctx context.Context, local, remote string) (bool, error) {
	rel, err := r.Compare(ctx, local, remote)
	return rel == Diverged, err
}

// IsAncestor is `git merge-base --is-ancestor a b`.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.git(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// FastForward moves the checked-out branch of this worktree to `to`, refusing anything but a fast-forward.
func (r *Repo) FastForward(ctx context.Context, to string) error {
	_, err := r.git(ctx, "merge", "--ff-only", "--quiet", to)
	return err
}

// RefExists reports whether a full ref (refs/heads/x, refs/remotes/origin/x) or a revision resolves.
func (r *Repo) RefExists(ctx context.Context, ref string) bool {
	_, err := r.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// RemoteHasBranch asks origin itself (ls-remote) whether it has the branch: the remote-tracking ref may be stale.
func (r *Repo) RemoteHasBranch(ctx context.Context, name string) (bool, error) {
	_, err := r.git(ctx, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+name)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 2 {
		return false, nil
	}
	return false, err
}

// CommitDate is the committer date of a revision.
func (r *Repo) CommitDate(ctx context.Context, rev string) (time.Time, error) {
	out, err := r.git(ctx, "show", "-s", "--format=%cI", rev+"^{commit}", "--")
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(out))
}

// IsIgnored reports whether a path (relative to the checkout) is gitignored.
func (r *Repo) IsIgnored(ctx context.Context, path string) bool {
	_, err := r.git(ctx, "check-ignore", "--quiet", "--", path)
	return err == nil
}

func (r *Repo) revParse(ctx context.Context, rev string) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", &GitError{Args: []string{"rev-parse", rev}, Stderr: "unknown revision " + rev, Err: err}
	}
	return strings.TrimSpace(out), nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// SamePath compares two paths after resolving symlinks where they exist (macOS temp dirs live behind /var →
// /private/var, and git prints the resolved form).
func SamePath(a, b string) bool {
	return Canonical(a) == Canonical(b)
}

// Canonical is the absolute path with symlinks resolved; a path that does not exist yet has its parent resolved.
func Canonical(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	// The path may not exist (yet): resolve its parent.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(parent, filepath.Base(abs))
	}
	return filepath.Clean(abs)
}

// Exists reports whether a path exists on disk.
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
