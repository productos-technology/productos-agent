package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/gitx"
)

// Entry is one worktree the agent keeps, with what prune needs to decide about it.
type Entry struct {
	Meta
	// Missing: the directory is gone; only its sidecar is left.
	Missing bool
	// Orphan: its checkout is gone, or git no longer lists the directory as one of the checkout's worktrees.
	Orphan bool
	// Dirty: uncommitted changes (gitignored files do not count).
	Dirty bool
	// Merged: the branch is in origin/<default> (its tip is an ancestor, or gh says its pull request was merged).
	Merged bool
	// MergedKnown: the merge could be checked at all.
	MergedKnown bool
}

// Options drive List and Prune.
type Options struct {
	Root string
	// GH is a signed-in gh: a squash-merged pull request counts as merged (its commits are not ancestors).
	GH string
	// Fetch origin/<default> of each checkout first, so a merge on GitHub is seen.
	Fetch bool
	Now   time.Time
	// UnusedAfter defaults to 14 days.
	UnusedAfter time.Duration
	// Force removes worktrees with uncommitted changes too (the CLI's --force; the daemon never forces).
	Force bool
	// DryRun says what Prune would remove without removing anything.
	DryRun bool
	// Skip says a run works in a worktree now: it is listed but never removed.
	Skip func(e Entry) bool
	// Lock is the repository lock (the Coordinator's), keyed by the checkout; nil locks nothing.
	Lock func(key string) func()
}

func (o *Options) defaults() {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.UnusedAfter <= 0 {
		o.UnusedAfter = UnusedAfter
	}
	if o.Lock == nil {
		o.Lock = func(string) func() { return func() {} }
	}
}

// List reads every worktree under the root (sidecars and bare directories), oldest use first.
func List(ctx context.Context, opts Options) ([]Entry, error) {
	opts.defaults()
	repoDirs, err := os.ReadDir(opts.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byPath := map[string]*Entry{}
	for _, rd := range repoDirs {
		if !rd.IsDir() {
			continue
		}
		dir := filepath.Join(opts.Root, rd.Name())
		items, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, it := range items {
			name := it.Name()
			switch {
			case it.IsDir():
				path := filepath.Join(dir, name)
				if byPath[path] == nil {
					byPath[path] = &Entry{Meta: Meta{Path: path}}
				}
			case strings.HasSuffix(name, ".json"):
				path := filepath.Join(dir, strings.TrimSuffix(name, ".json"))
				m, err := ReadMeta(path)
				if err != nil || m == nil {
					continue
				}
				m.Path = path
				byPath[path] = &Entry{Meta: *m}
			}
		}
	}
	fetched := map[string]bool{}
	out := make([]Entry, 0, len(byPath))
	for _, e := range byPath {
		inspect(ctx, e, opts, fetched)
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsedAt.Before(out[j].LastUsedAt) })
	return out, nil
}

// inspect fills what the sidecar does not know: whether the directory and its checkout exist, the branch, the last
// use, dirty, merged.
func inspect(ctx context.Context, e *Entry, opts Options, fetched map[string]bool) {
	info, err := os.Stat(e.Path)
	e.Missing = err != nil
	if e.LastUsedAt.IsZero() && info != nil {
		e.LastUsedAt = info.ModTime()
	}
	var wt *gitx.Repo
	if !e.Missing {
		if r, err := gitx.Open(e.Path); err == nil && gitx.SamePath(r.Dir, e.Path) {
			wt = r
		}
		if wt != nil && e.Checkout == "" {
			e.Checkout, _ = wt.MainCheckout(ctx)
		}
		if wt != nil && e.Branch == "" {
			if b, err := wt.CurrentBranch(ctx); err == nil && b != "HEAD" {
				e.Branch = b
			}
		}
	}
	checkout, err := gitx.Open(e.Checkout)
	if e.Checkout == "" || err != nil {
		e.Orphan = true
		return
	}
	if !e.Missing {
		list, err := checkout.WorktreeList(ctx)
		listed := false
		for _, w := range list {
			if gitx.SamePath(w.Path, e.Path) {
				listed = true
			}
		}
		if err != nil || !listed || wt == nil {
			e.Orphan = true
			return
		}
		clean, err := wt.IsClean(ctx)
		e.Dirty = err == nil && !clean
	}
	if e.Branch == "" {
		return
	}
	if e.DefaultBranch == "" {
		e.DefaultBranch, _ = checkout.DefaultBranch(ctx)
	}
	if e.DefaultBranch == "" {
		return
	}
	if opts.Fetch && !fetched[checkout.Dir] {
		fetched[checkout.Dir] = true
		unlock := opts.Lock(checkout.Dir)
		fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_ = checkout.Fetch(fctx, e.DefaultBranch)
		cancel()
		unlock()
	}
	base := "refs/remotes/origin/" + e.DefaultBranch
	tip := "refs/heads/" + e.Branch
	if !checkout.RefExists(ctx, tip) || !checkout.RefExists(ctx, base) {
		return
	}
	merged, err := checkout.IsAncestor(ctx, tip, base)
	if err != nil {
		return
	}
	e.MergedKnown = true
	e.Merged = merged
	if !merged && opts.GH != "" && e.Repository != "" {
		e.Merged = prMerged(ctx, opts.GH, e.Repository, e.Branch)
	}
}

// prMerged asks gh whether a merged pull request has this head branch (squash and rebase merges).
func prMerged(ctx context.Context, gh, repo, branch string) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "pr", "list", "--repo", repo, "--head", branch, "--state", "merged", "--json", "number", "--limit", "1")
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var prs []struct {
		Number int `json:"number"`
	}
	return json.Unmarshal(out, &prs) == nil && len(prs) > 0
}

// Outcome is what Prune did with one worktree.
type Outcome struct {
	Entry
	Removed bool
	// Reason says why it was removed or kept.
	Reason string
	Err    error
}

// Prune removes the worktrees whose branch is merged into origin/<default> or that nobody used for UnusedAfter,
// and the leftovers of worktrees git forgot. It never removes one a run uses (Skip), nor one with uncommitted
// changes unless Force, and never deletes a branch: only the directory goes.
func Prune(ctx context.Context, opts Options) ([]Outcome, error) {
	opts.defaults()
	entries, err := List(ctx, opts)
	if err != nil {
		return nil, err
	}
	var out []Outcome
	for _, e := range entries {
		o := Outcome{Entry: e}
		stale := opts.Now.Sub(e.LastUsedAt) > opts.UnusedAfter
		switch {
		case opts.Skip != nil && opts.Skip(e):
			o.Reason = "a run is using it"
		case !Inside(opts.Root, e.Path):
			o.Reason = "outside the agent's directory"
		case e.Missing:
			o.Removed, o.Reason = true, "its directory is gone"
			if opts.DryRun {
				break
			}
			_ = os.Remove(metaFile(e.Path))
			if checkout, err := gitx.Open(e.Checkout); err == nil {
				unlock := opts.Lock(checkout.Dir)
				_ = checkout.WorktreePrune(ctx)
				unlock()
			}
		case e.Orphan:
			if !stale && gitx.Exists(e.Checkout) {
				o.Reason = "not a worktree of its checkout; removed once unused for 14 days"
				break
			}
			o.Reason = "left over from a checkout that is gone"
			if gitx.Exists(e.Checkout) {
				o.Reason = "left over, unused for 14 days"
			}
			if opts.DryRun {
				o.Removed = true
				break
			}
			if o.Err = os.RemoveAll(e.Path); o.Err == nil {
				_ = os.Remove(metaFile(e.Path))
				o.Removed = true
			}
		case !e.Merged && !stale:
			o.Reason = "in use"
		case e.Dirty && !opts.Force:
			o.Reason = "uncommitted changes (prune --force removes it)"
		default:
			o.Reason = "unused for 14 days"
			if e.Merged {
				o.Reason = "merged into origin/" + e.DefaultBranch
			}
			if opts.DryRun {
				o.Removed = true
				break
			}
			o.Removed, o.Err = remove(ctx, e, opts)
			if !o.Removed && o.Err == nil {
				o.Reason = "a run is using it"
			}
		}
		out = append(out, o)
	}
	if !opts.DryRun {
		cleanEmptyDirs(opts.Root)
	}
	return out, nil
}

// remove takes the worktree away under the repository lock, asking Skip again once it holds it: a run claims its
// directory before it takes the lock, so a run that started meanwhile is seen here.
func remove(ctx context.Context, e Entry, opts Options) (bool, error) {
	checkout, err := gitx.Open(e.Checkout)
	if err != nil {
		return false, err
	}
	unlock := opts.Lock(checkout.Dir)
	defer unlock()
	if opts.Skip != nil && opts.Skip(e) {
		return false, nil
	}
	if err := checkout.WorktreeRemove(ctx, e.Path, opts.Force); err != nil {
		return false, err
	}
	_ = os.Remove(metaFile(e.Path))
	_ = checkout.WorktreePrune(ctx)
	return true, nil
}

// cleanEmptyDirs removes repository directories left empty.
func cleanEmptyDirs(root string) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if d.IsDir() {
			_ = os.Remove(filepath.Join(root, d.Name())) // fails unless empty
		}
	}
}
