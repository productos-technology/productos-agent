package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/productos-technology/productos-agent/internal/config"
	"github.com/productos-technology/productos-agent/internal/logx"
	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/worktree"
)

type recordingReporter struct {
	mu     sync.Mutex
	events []protocol.Event
}

func (r *recordingReporter) Events(_ context.Context, _ string, events []protocol.Event) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	return len(r.events) + 1, nil
}

func (r *recordingReporter) Status(context.Context, string, protocol.StatusReport) error { return nil }

func (r *recordingReporter) logs() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		if p, ok := e.Payload.(protocol.LogPayload); ok {
			b.WriteString(p.Level + ": " + p.Text + "\n")
		}
	}
	return b.String()
}

func gitT(t *testing.T, dir string, args ...string) string {
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

// repoFixture is a bare origin with one commit on main and the person's clone of it.
type repoFixture struct {
	root, origin, work string
}

func newRepoFixture(t *testing.T) repoFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("PRODUCTOS_AGENT_STATE_DIR", t.TempDir())
	root := t.TempDir()
	f := repoFixture{root: root, origin: filepath.Join(root, "origin.git"), work: filepath.Join(root, "mobile")}
	gitT(t, root, "init", "--bare", "-b", "main", f.origin)
	gitT(t, root, "clone", "-q", f.origin, f.work)
	gitT(t, f.work, "config", "user.name", "t")
	gitT(t, f.work, "config", "user.email", "t@example.com")
	gitT(t, f.work, "checkout", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(f.work, "README.md"), []byte("# Mobile\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f.work, ".gitignore"), []byte(".env\nconfig/local/\nnode_modules/\n"), 0o644)
	gitT(t, f.work, "add", "-A")
	gitT(t, f.work, "commit", "-q", "-m", "init")
	gitT(t, f.work, "push", "-q", "-u", "origin", "main")
	gitT(t, f.work, "remote", "set-head", "origin", "main")
	return f
}

// pushElsewhere commits a file on branch from another clone and pushes it (someone else, or the previous run).
func (f repoFixture) pushElsewhere(t *testing.T, branch, file, content string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, f.root, "clone", "-q", f.origin, other)
	switch {
	case gitT(t, other, "rev-parse", "--abbrev-ref", "HEAD") == branch:
	case strings.Contains(gitT(t, other, "ls-remote", "--heads", "origin", branch), branch):
		gitT(t, other, "checkout", "-q", "-b", branch, "origin/"+branch)
	default:
		gitT(t, other, "checkout", "-q", "-b", branch)
	}
	_ = os.WriteFile(filepath.Join(other, file), []byte(content), 0o644)
	gitT(t, other, "add", "-A")
	gitT(t, other, "commit", "-q", "-m", "change "+file)
	gitT(t, other, "push", "-q", "origin", branch)
	return gitT(t, other, "rev-parse", "HEAD")
}

func (f repoFixture) start(id, branch string, wt bool, cont *protocol.Continue) protocol.RunStart {
	return protocol.RunStart{RunID: id, Tool: "claude", Branch: branch, Continue: cont,
		Subject:    protocol.Subject{Kind: "change", Key: "MOB-C4", Name: "Keep offline drafts"},
		Repository: protocol.RunRepository{ID: "repo-1", FullName: "acme/mobile", LocalPath: f.work, DefaultBranch: "main"},
		Options:    protocol.RunOptions{CommitTrailer: "ProductOS: MOB-C4", OpenPR: true, RunTests: true, Worktree: wt}}
}

func newPreparedRun(t *testing.T, start protocol.RunStart, cfg *config.Config) (*Run, *recordingReporter) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	rep := &recordingReporter{}
	r := New(start, rep, cfg, logx.Discard())
	r.emitter = newEmitter(start.RunID, rep, r.Log)
	t.Cleanup(func() {
		if r.release != nil {
			r.release()
		}
	})
	return r, rep
}

func prepareRun(t *testing.T, start protocol.RunStart, cfg *config.Config) (*Run, string, error, string) {
	t.Helper()
	r, rep := newPreparedRun(t, start, cfg)
	code, err := r.prepare(context.Background())
	r.emitter.Close(context.Background())
	return r, code, err, rep.logs()
}

const branch = "productos/mob-c4-keep-offline-drafts"

func TestPrepareFreshRunMakesAWorktreeAndLeavesTheCheckoutAlone(t *testing.T) {
	f := newRepoFixture(t)
	// The person is in the middle of something: a modified file, an untracked one, and secrets git ignores.
	_ = os.WriteFile(filepath.Join(f.work, "README.md"), []byte("# Mobile (editing)\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f.work, "scratch.txt"), []byte("mine\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f.work, ".env"), []byte("TOKEN=local\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(f.work, "config", "local"), 0o755)
	_ = os.WriteFile(filepath.Join(f.work, "config", "local", "db.yml"), []byte("dev: {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f.work, ".worktreeinclude"), []byte("# what a worktree needs\n.env\n/config/local/\nREADME.md\n../outside\n!negated\n"), 0o644)
	before := gitT(t, f.work, "status", "--porcelain")

	r, code, err, logs := prepareRun(t, f.start("run-1", branch, true, nil), nil)
	if err != nil {
		t.Fatalf("prepare: %s %v\n%s", code, err, logs)
	}
	root, _ := worktree.Root()
	want := filepath.Join(root, "repo-1", "productos-mob-c4-keep-offline-drafts")
	if r.worktree != want || r.repo.Dir != want {
		t.Fatalf("worktree = %q, want %q", r.worktree, want)
	}
	if got := gitT(t, want, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("worktree branch = %q", got)
	}
	if r.base != gitT(t, f.work, "rev-parse", "origin/main") {
		t.Error("a fresh branch starts at origin/main")
	}
	if gitT(t, f.work, "rev-parse", "--abbrev-ref", "HEAD") != "main" || gitT(t, f.work, "status", "--porcelain") != before {
		t.Error("the person's checkout moved or changed")
	}
	if got, _ := os.ReadFile(filepath.Join(want, ".env")); string(got) != "TOKEN=local\n" {
		t.Errorf(".env not copied: %q", got)
	}
	if _, err := os.Stat(filepath.Join(want, "config", "local", "db.yml")); err != nil {
		t.Error("an ignored directory named in .worktreeinclude is copied")
	}
	if got, _ := os.ReadFile(filepath.Join(want, "README.md")); string(got) != "# Mobile\n" {
		t.Errorf("a tracked file is never copied over the branch's: %q", got)
	}
	if _, err := os.Stat(filepath.Join(want, "scratch.txt")); err == nil {
		t.Error("untracked work that is not named is not copied")
	}
	if !strings.Contains(logs, "Copied 2 file(s) named in .worktreeinclude") {
		t.Errorf("logs = %s", logs)
	}
	info, _ := os.Stat(filepath.Dir(want))
	if info == nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the worktrees directory is owner-only: %v", info)
	}
	m, err := worktree.ReadMeta(want)
	if err != nil || m == nil || m.Branch != branch || m.LastRunID != "run-1" || m.Checkout == "" || m.DefaultBranch != "main" {
		t.Errorf("meta = %+v (%v)", m, err)
	}
	if r.preparedLine() != "Branch `"+branch+"` created from `origin/main` in the worktree "+want+"." {
		t.Errorf("line = %q", r.preparedLine())
	}
}

func TestPrepareFreshRunRefusesATakenNameNeverSuffixes(t *testing.T) {
	for _, wt := range []bool{true, false} {
		t.Run(map[bool]string{true: "worktree", false: "checkout"}[wt], func(t *testing.T) {
			f := newRepoFixture(t)
			f.pushElsewhere(t, branch, "a.txt", "a\n")
			_, code, err, _ := prepareRun(t, f.start("run-2", branch, wt, nil), nil)
			if code != codeBranchFailed || err == nil || !strings.Contains(err.Error(), "already exists on origin") {
				t.Errorf("remote: %s %v", code, err)
			}
			// A local branch with work of its own (an empty one is an earlier run's leftover, reused below).
			gitT(t, f.work, "checkout", "-q", "-b", "local-only")
			_ = os.WriteFile(filepath.Join(f.work, "local.txt"), []byte("mine\n"), 0o644)
			gitT(t, f.work, "add", "-A")
			gitT(t, f.work, "commit", "-q", "-m", "local work")
			gitT(t, f.work, "checkout", "-q", "main")
			_, code, err, _ = prepareRun(t, f.start("run-3", "local-only", wt, nil), nil)
			if code != codeBranchFailed || err == nil || !strings.Contains(err.Error(), "already exists on this machine") {
				t.Errorf("local: %s %v", code, err)
			}
			if out := gitT(t, f.work, "branch", "--list", "*-2"); out != "" {
				t.Errorf("a -N branch was made: %q", out)
			}
		})
	}
}

func TestPrepareRefusesTheBranchOfThePersonsCheckout(t *testing.T) {
	f := newRepoFixture(t)
	f.pushElsewhere(t, branch, "a.txt", "a\n")
	gitT(t, f.work, "fetch", "-q", "origin", branch)
	gitT(t, f.work, "checkout", "-q", "-b", branch, "origin/"+branch)
	_, code, err, _ := prepareRun(t, f.start("run-4", branch, true, &protocol.Continue{Branch: branch}), nil)
	if code != codeBranchCheckedOut || err == nil || !strings.Contains(err.Error(), "checked out in your checkout") {
		t.Errorf("adjust: %s %v", code, err)
	}
	_, code, _, _ = prepareRun(t, f.start("run-5", branch, true, nil), nil)
	if code != codeBranchCheckedOut {
		t.Errorf("fresh: %s", code)
	}
}

func TestPrepareAdjustTakesTheBranchFromOrigin(t *testing.T) {
	f := newRepoFixture(t)
	head := f.pushElsewhere(t, branch, "drafts.ts", "export {}\n")
	r, code, err, logs := prepareRun(t, f.start("run-6", "", true, &protocol.Continue{Branch: branch, HeadCommit: head}), nil)
	if err != nil {
		t.Fatalf("%s %v\n%s", code, err, logs)
	}
	if r.base != head || gitT(t, r.worktree, "rev-parse", "--abbrev-ref", "HEAD") != branch {
		t.Errorf("base = %s, want %s", r.base, head)
	}
	if !strings.HasPrefix(r.preparedLine(), "Continuing `"+branch+"` at "+head[:7]+" in the worktree ") {
		t.Errorf("line = %q", r.preparedLine())
	}
	if gitT(t, f.work, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Error("the person's checkout stays on main")
	}
}

func TestPrepareAdjustReusesTheWorktreeAndFastForwards(t *testing.T) {
	f := newRepoFixture(t)
	first, code, err, _ := prepareRun(t, f.start("run-7", branch, true, nil), nil)
	if err != nil {
		t.Fatalf("%s %v", code, err)
	}
	_ = os.WriteFile(filepath.Join(first.worktree, "round1.txt"), []byte("1\n"), 0o644)
	gitT(t, first.worktree, "add", "-A")
	gitT(t, first.worktree, "commit", "-q", "-m", "round 1")
	gitT(t, first.worktree, "push", "-q", "-u", "origin", branch)
	// A reviewer pushed a fix on the branch meanwhile.
	theirs := f.pushElsewhere(t, branch, "review.txt", "fix\n")

	second, code, err, logs := prepareRun(t, f.start("run-8", branch, true, &protocol.Continue{Branch: branch}), nil)
	if err != nil {
		t.Fatalf("%s %v\n%s", code, err, logs)
	}
	if second.worktree != first.worktree || second.base != theirs {
		t.Errorf("worktree %q base %s, want %q at %s", second.worktree, second.base, first.worktree, theirs)
	}
	if !strings.Contains(logs, "Fast-forwarded `"+branch+"` to origin.") {
		t.Errorf("logs = %s", logs)
	}
}

func TestPrepareAdjustRefusesADivergedBranch(t *testing.T) {
	f := newRepoFixture(t)
	first, _, err, _ := prepareRun(t, f.start("run-9", branch, true, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.pushElsewhere(t, branch, "theirs.txt", "theirs\n")
	_ = os.WriteFile(filepath.Join(first.worktree, "mine.txt"), []byte("mine\n"), 0o644)
	gitT(t, first.worktree, "add", "-A")
	gitT(t, first.worktree, "commit", "-q", "-m", "not pushed")
	local := gitT(t, first.worktree, "rev-parse", "HEAD")

	r, code, err, _ := prepareRun(t, f.start("run-10", branch, true, &protocol.Continue{Branch: branch}), nil)
	if code != codeBranchDiverged || err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("%s %v", code, err)
	}
	if r.base != "" || gitT(t, first.worktree, "rev-parse", "HEAD") != local {
		t.Error("nothing ran and nothing moved")
	}
}

func TestPrepareAdjustCommitsWhatAnInterruptedRunLeft(t *testing.T) {
	f := newRepoFixture(t)
	first, _, err, _ := prepareRun(t, f.start("run-11", branch, true, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(first.worktree, "half.ts"), []byte("// half\n"), 0o644)
	r, code, err, logs := prepareRun(t, f.start("run-12", branch, true, &protocol.Continue{Branch: branch}), nil)
	if err != nil {
		t.Fatalf("%s %v", code, err)
	}
	if msg := gitT(t, r.worktree, "log", "-1", "--format=%s"); msg != "WIP: MOB-C4: Keep offline drafts" || gitT(t, r.worktree, "status", "--porcelain") != "" {
		t.Errorf("last commit %q", msg)
	}
	if !strings.Contains(logs, "interrupted earlier run") {
		t.Errorf("logs = %s", logs)
	}
}

func TestPrepareAdjustOfABranchThatExistsNowhere(t *testing.T) {
	f := newRepoFixture(t)
	_, code, err, _ := prepareRun(t, f.start("run-13", branch, true, &protocol.Continue{Branch: branch}), nil)
	if code != codeBranchFailed || err == nil || !strings.Contains(err.Error(), "exists neither") {
		t.Errorf("%s %v", code, err)
	}
}

func TestPrepareWithWorktreesOffStaysInTheCheckout(t *testing.T) {
	f := newRepoFixture(t)
	off := false
	r, code, err, logs := prepareRun(t, f.start("run-14", branch, true, nil), &config.Config{Worktrees: &off})
	if err != nil {
		t.Fatalf("%s %v", code, err)
	}
	if r.worktree != "" || r.repo.Dir != r.main.Dir || gitT(t, f.work, "rev-parse", "--abbrev-ref", "HEAD") != branch {
		t.Errorf("worktree %q dir %q", r.worktree, r.repo.Dir)
	}
	if !strings.Contains(logs, "Worktrees are off in config.json") {
		t.Errorf("logs = %s", logs)
	}
	// And an adjust run in the checkout checks the branch out and catches up.
	gitT(t, f.work, "checkout", "-q", "main")
	theirs := f.pushElsewhere(t, branch, "b.txt", "b\n")
	r, code, err, _ = prepareRun(t, f.start("run-15", branch, false, &protocol.Continue{Branch: branch}), nil)
	if err != nil {
		t.Fatalf("%s %v", code, err)
	}
	if r.base != theirs || gitT(t, f.work, "rev-parse", "--abbrev-ref", "HEAD") != branch {
		t.Errorf("base %s want %s", r.base, theirs)
	}
}

func TestTwoRunsNeverShareADirectory(t *testing.T) {
	f := newRepoFixture(t)
	coord := worktree.NewCoordinator()
	a, _ := newPreparedRun(t, f.start("run-a", branch, true, nil), nil)
	a.Coord = coord
	if code, err := a.prepare(context.Background()); err != nil {
		t.Fatalf("%s %v", code, err)
	}
	b, _ := newPreparedRun(t, f.start("run-b", branch, true, &protocol.Continue{Branch: branch}), nil)
	b.Coord = coord
	code, err := b.prepare(context.Background())
	if code != codeInternal || err == nil || !strings.Contains(err.Error(), "run-a") {
		t.Errorf("%s %v", code, err)
	}
	a.release()
	a.release = nil
	b.release = nil
	if code, err := b.prepare(context.Background()); err != nil {
		t.Errorf("once released: %s %v", code, err)
	}
}

// A fresh run that failed before committing anything left its branch at origin/main, here only: the next fresh run
// reuses it (nothing to lose) instead of refusing a dead end.
func TestPrepareFreshRunReusesAnEmptyLeftoverBranch(t *testing.T) {
	for _, wt := range []bool{true, false} {
		t.Run(map[bool]string{true: "worktree", false: "checkout"}[wt], func(t *testing.T) {
			f := newRepoFixture(t)
			first, code, err, _ := prepareRun(t, f.start("run-l1", branch, wt, nil), nil)
			if err != nil {
				t.Fatalf("%s %v", code, err)
			}
			if !wt {
				gitT(t, f.work, "checkout", "-q", "main")
			}
			f.pushElsewhere(t, "main", "later.txt", "main moved on\n")
			second, code, err, logs := prepareRun(t, f.start("run-l2", branch, wt, nil), nil)
			if err != nil {
				t.Fatalf("%s %v\n%s", code, err, logs)
			}
			if second.base != gitT(t, f.work, "rev-parse", "origin/main") || second.repo.Dir != first.repo.Dir {
				t.Errorf("base %s, dir %s", second.base, second.repo.Dir)
			}
			if !strings.Contains(logs, "Reusing `"+branch+"`: an earlier run left it without a commit.") {
				t.Errorf("logs = %s", logs)
			}
		})
	}
}
