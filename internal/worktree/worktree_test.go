package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/productos-technology/productos-agent/internal/gitx"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func checkout(t *testing.T) (origin, work string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	origin, work = filepath.Join(root, "origin.git"), filepath.Join(root, "mobile")
	git(t, root, "init", "--bare", "-b", "main", origin)
	git(t, root, "clone", "-q", origin, work)
	git(t, work, "checkout", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(work, "README.md"), []byte("# Mobile\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, ".gitignore"), []byte(".env\nnode_modules/\n"), 0o644)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "init")
	git(t, work, "push", "-q", "-u", "origin", "main")
	git(t, work, "remote", "set-head", "origin", "main")
	return origin, work
}

func TestPathForAndInside(t *testing.T) {
	root := filepath.Join("/state", "worktrees")
	got := PathFor(root, "0199a1b2-c3d4", "acme/mobile", "productos/MOB-C4-keep-offline-drafts")
	if got != filepath.Join(root, "0199a1b2-c3d4", "productos-mob-c4-keep-offline-drafts") {
		t.Errorf("path = %q", got)
	}
	if PathFor(root, "", "acme/mobile", "x") != filepath.Join(root, "acme-mobile", "x") {
		t.Error("without an id, the full name")
	}
	if !Inside(root, got) || Inside(root, root) || Inside(root, "/state/other") || Inside(root, filepath.Join(root, "..", "x")) {
		t.Error("inside")
	}
}

func TestCoordinatorClaims(t *testing.T) {
	c := NewCoordinator()
	dir := t.TempDir()
	release, _, ok := c.Claim(dir, "run-1")
	if !ok || !c.Claimed(dir) {
		t.Fatal("first claim")
	}
	if _, holder, ok := c.Claim(dir+string(filepath.Separator), "run-2"); ok || holder != "run-1" {
		t.Errorf("second claim: %v %q", ok, holder)
	}
	if dirs := c.ClaimedDirs(); len(dirs) != 1 {
		t.Errorf("dirs = %v", dirs)
	}
	release()
	if c.Claimed(dir) {
		t.Error("released")
	}
	unlock := c.LockRepo(dir)
	done := make(chan struct{})
	go func() { c.LockRepo(dir)(); close(done) }()
	select {
	case <-done:
		t.Fatal("the repository lock let a second holder in")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-done
}

func TestCopyIncludedOnlyIgnoredAndOnlyMissing(t *testing.T) {
	_, work := checkout(t)
	_ = os.WriteFile(filepath.Join(work, ".env"), []byte("A=1\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(work, "node_modules", "left-pad"), 0o755)
	_ = os.WriteFile(filepath.Join(work, "node_modules", "left-pad", "index.js"), []byte("module.exports = 1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, IncludeFile), []byte(".env\nnode_modules\nREADME.md\n*.missing\n"), 0o644)
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	n, err := CopyIncluded(context.Background(), repo, dst, false)
	if err != nil || n != 2 {
		t.Fatalf("copied %d (%v)", n, err)
	}
	if info, err := os.Stat(filepath.Join(dst, ".env")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf(".env: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err == nil {
		t.Error("a tracked file is not copied")
	}
	_ = os.WriteFile(filepath.Join(dst, ".env"), []byte("A=changed-by-the-run\n"), 0o600)
	if n, _ := CopyIncluded(context.Background(), repo, dst, true); n != 0 {
		t.Errorf("only missing: copied %d", n)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, ".env")); string(got) != "A=changed-by-the-run\n" {
		t.Error("an existing file stays")
	}
}

// addWorktree makes a worktree like a run does, with its sidecar.
func addWorktree(t *testing.T, root, work, branch string, lastUsed time.Time) string {
	t.Helper()
	path := PathFor(root, "repo-1", "acme/mobile", branch)
	if err := Ensure(path); err != nil {
		t.Fatal(err)
	}
	git(t, work, "worktree", "add", "-q", "--no-track", "-b", branch, path, "origin/main")
	_ = os.WriteFile(filepath.Join(path, strings.ReplaceAll(branch, "/", "-")+".txt"), []byte(branch+"\n"), 0o644)
	git(t, path, "add", "-A")
	git(t, path, "commit", "-q", "-m", branch)
	if err := WriteMeta(Meta{Path: path, Branch: branch, RepositoryID: "repo-1", Repository: "acme/mobile", Checkout: work, DefaultBranch: "main", LastUsedAt: lastUsed, LastRunID: "run-" + branch}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListAndPrune(t *testing.T) {
	_, work := checkout(t)
	root := filepath.Join(t.TempDir(), "worktrees")
	now := time.Now()
	old := now.Add(-15 * 24 * time.Hour)

	merged := addWorktree(t, root, work, "productos/merged", now)
	git(t, merged, "push", "-q", "origin", "HEAD:main") // merged on GitHub
	active := addWorktree(t, root, work, "productos/active", now)
	stale := addWorktree(t, root, work, "productos/stale", old)
	dirty := addWorktree(t, root, work, "productos/dirty", old)
	_ = os.WriteFile(filepath.Join(dirty, "unsaved.txt"), []byte("x\n"), 0o644)
	busy := addWorktree(t, root, work, "productos/busy", old)
	orphan := filepath.Join(root, "repo-1", "orphan")
	_ = os.MkdirAll(orphan, 0o700)
	_ = os.WriteFile(filepath.Join(orphan, "file"), []byte("x"), 0o644)
	_ = WriteMeta(Meta{Path: orphan, Branch: "productos/orphan", Checkout: filepath.Join(t.TempDir(), "gone"), LastUsedAt: now})
	missing := filepath.Join(root, "repo-1", "missing")
	_ = WriteMeta(Meta{Path: missing, Branch: "productos/missing", Checkout: work, LastUsedAt: now})

	opts := Options{Root: root, Fetch: true, Now: now, Skip: func(e Entry) bool { return e.Path == busy }}
	entries, err := List(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if len(entries) != 7 {
		t.Fatalf("entries = %d: %+v", len(entries), entries)
	}
	if e := byPath[merged]; !e.Merged || !e.MergedKnown || e.Orphan || e.Dirty {
		t.Errorf("merged = %+v", e)
	}
	if e := byPath[active]; e.Merged || !e.MergedKnown {
		t.Errorf("active = %+v", e)
	}
	if e := byPath[dirty]; !e.Dirty {
		t.Errorf("dirty = %+v", e)
	}
	if e := byPath[orphan]; !e.Orphan {
		t.Errorf("orphan = %+v", e)
	}
	if e := byPath[missing]; !e.Missing {
		t.Errorf("missing = %+v", e)
	}

	dry := opts
	dry.DryRun = true
	if _, err := Prune(context.Background(), dry); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{merged, stale, orphan} {
		if !gitx.Exists(p) {
			t.Errorf("a dry run removed %s", p)
		}
	}

	outcomes, err := Prune(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]string{}
	kept := map[string]string{}
	for _, o := range outcomes {
		if o.Err != nil {
			t.Errorf("%s: %v", o.Path, o.Err)
		}
		if o.Removed {
			removed[o.Path] = o.Reason
		} else {
			kept[o.Path] = o.Reason
		}
	}
	if removed[merged] != "merged into origin/main" || removed[stale] != "unused for 14 days" || removed[orphan] == "" || removed[missing] == "" {
		t.Errorf("removed = %v", removed)
	}
	if kept[active] != "in use" || !strings.Contains(kept[dirty], "uncommitted changes") || kept[busy] != "a run is using it" {
		t.Errorf("kept = %v", kept)
	}
	for _, p := range []string{merged, stale, orphan} {
		if gitx.Exists(p) || gitx.Exists(p+".json") {
			t.Errorf("%s is still there", p)
		}
	}
	if gitx.Exists(missing + ".json") {
		t.Error("the sidecar of a gone directory is dropped")
	}
	for _, b := range []string{"productos/merged", "productos/stale"} {
		git(t, work, "rev-parse", "--verify", "refs/heads/"+b) // branches are never deleted
	}
	if list := git(t, work, "worktree", "list"); strings.Contains(list, merged) || !strings.Contains(list, active) {
		t.Errorf("git worktree list:\n%s", list)
	}

	opts.Force = true
	outcomes, _ = Prune(context.Background(), opts)
	for _, o := range outcomes {
		if o.Path == dirty && !o.Removed {
			t.Errorf("--force removes a dirty stale worktree: %+v", o)
		}
	}
}
