package runner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/gitx"
	"github.com/productos-technology/productos-agent/internal/worktree"
)

// Where a run works, and on which branch (docs/reference/agent-protocol.md, Continuing a run and worktrees):
//
//   - options.worktree (and worktrees on in config.json): the change's own worktree under
//     <StateDir>/worktrees/<repository_id>/<branch>. The person's checkout is never touched and may be dirty.
//   - otherwise the linked checkout itself, which must be clean, as before worktrees.
//
// A fresh run (no `continue`) creates the branch from origin/<default>; a name that exists, here or on origin, is a
// refusal (branch_failed), never a `-2`. An adjust run (`continue`) takes the existing branch: created locally from
// origin when it is only there, fast-forwarded when behind, refused when it diverged (branch_diverged). Both refuse a
// branch checked out in the person's checkout (branch_checked_out): a worktree cannot share it.

// prepare opens the person's checkout, picks the branch and sets the run up where it works. The code says what failed.
func (r *Run) prepare(ctx context.Context) (string, error) {
	main, err := gitx.Open(r.Start.Repository.LocalPath)
	if err != nil {
		return codeCheckoutMissing, err
	}
	r.main = main
	from := r.Start.Repository.DefaultBranch
	if from == "" {
		if from, err = main.DefaultBranch(ctx); err != nil {
			return codeFetchFailed, err
		}
	}
	r.from = from
	name := r.branchName()
	if r.Start.Options.Worktree && !r.Config.WorktreesEnabled() {
		r.info("Worktrees are off in config.json: this run works in the linked checkout.")
	}
	if r.useWorktree() {
		return r.prepareWorktree(ctx, name)
	}
	return r.prepareCheckout(ctx, name)
}

// branchName: the adjust run's branch, else the server's, else the fallback built from the subject.
func (r *Run) branchName() string {
	if c := r.Start.Continue; c != nil {
		if b := strings.TrimSpace(c.Branch); b != "" && gitx.ValidBranch(b) {
			return b
		}
	}
	if b := strings.TrimSpace(r.Start.Branch); b != "" && gitx.ValidBranch(b) {
		return b
	}
	return gitx.BranchName(r.Start.Subject.Key, r.Start.Subject.Name)
}

func (r *Run) useWorktree() bool { return r.Start.Options.Worktree && r.Config.WorktreesEnabled() }

func (r *Run) adjusting() bool { return r.Start.Continue != nil }

// claim keeps every other run of this device out of dir until the run ends.
func (r *Run) claim(dir string) error {
	release, holder, ok := r.coord().Claim(dir, r.Start.RunID)
	if !ok {
		return fmt.Errorf("another run (%s) is working in %s", holder, dir)
	}
	r.release = release
	return nil
}

func (r *Run) lockRepo() func() { return r.coord().LockRepo(r.main.Dir) }

func (r *Run) coord() *worktree.Coordinator {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Coord == nil {
		r.Coord = worktree.NewCoordinator()
	}
	return r.Coord
}

// prepareCheckout is the run in the person's checkout: clean tree, fetch, then a new branch from origin/<default> or,
// for an adjust run, the existing branch checked out and caught up.
func (r *Run) prepareCheckout(ctx context.Context, name string) (string, error) {
	main := r.main
	if err := r.claim(main.Dir); err != nil {
		return codeInternal, err
	}
	clean, err := main.IsClean(ctx)
	if err != nil {
		return codeInternal, err
	}
	if !clean {
		return codeCheckoutDirty, fmt.Errorf("the checkout at %s has uncommitted changes; commit or stash them and run again", main.Dir)
	}
	unlock := r.lockRepo()
	defer unlock()
	if err := main.Fetch(ctx, r.from); err != nil {
		return codeFetchFailed, err
	}
	if r.adjusting() {
		remote, code, err := r.fetchBranch(ctx, name)
		if err != nil {
			return code, err
		}
		if at, out, err := main.IsBranchCheckedOut(ctx, name); err == nil && out && !gitx.SamePath(at, main.Dir) {
			return codeBranchCheckedOut, fmt.Errorf("the branch `%s` is checked out in the worktree at %s; switch that one to another branch and run again", name, at)
		}
		current, _ := main.CurrentBranch(ctx)
		switch {
		case current == name:
		case main.BranchExists(ctx, name):
			if _, err := main.Git(ctx, "checkout", "--quiet", name); err != nil {
				return codeBranchFailed, err
			}
		default:
			if err := main.CreateBranch(ctx, name, name); err != nil {
				return codeBranchFailed, err
			}
		}
		r.repo, r.branch = main, name
		if code, err := r.catchUp(ctx, remote); err != nil {
			return code, err
		}
	} else {
		code, err := r.refuseTakenName(ctx, name)
		switch {
		case err != nil && !r.emptyLeftover(ctx, name):
			return code, err
		case err != nil:
			if current, _ := main.CurrentBranch(ctx); current != name {
				if _, err := main.Git(ctx, "checkout", "--quiet", name); err != nil {
					return codeBranchFailed, err
				}
			}
			r.repo, r.branch = main, name
			if code, err := r.reuseLeftover(ctx); err != nil {
				return code, err
			}
		default:
			if err := main.CreateBranch(ctx, name, r.from); err != nil {
				return codeBranchFailed, err
			}
			r.repo, r.branch = main, name
		}
	}
	if r.base, err = r.repo.Head(ctx); err != nil {
		return codeInternal, err
	}
	return "", nil
}

// prepareWorktree is the run in the change's own worktree. The person's checkout only lends its .git: its branch,
// its files and its uncommitted work stay as they are.
func (r *Run) prepareWorktree(ctx context.Context, name string) (string, error) {
	main := r.main
	root, err := worktree.Root()
	if err != nil {
		return codeWorktreeFailed, err
	}
	path := worktree.PathFor(root, r.Start.Repository.ID, r.Start.Repository.FullName, name)
	if err := r.claim(path); err != nil {
		return codeInternal, err
	}
	if err := worktree.Ensure(path); err != nil {
		return codeWorktreeFailed, fmt.Errorf("cannot create %s: %w", path, err)
	}

	created, code, err := r.addWorktree(ctx, name, path)
	if err != nil {
		return code, err
	}
	if r.base, err = r.repo.Head(ctx); err != nil {
		return codeInternal, err
	}
	if n, err := worktree.CopyIncluded(ctx, main, r.repo.Dir, !created); err != nil {
		r.warn("Could not copy every file named in .worktreeinclude: " + oneLine(err.Error(), 300))
	} else if n > 0 {
		r.info(fmt.Sprintf("Copied %d file(s) named in .worktreeinclude from your checkout.", n))
	}
	r.touchMeta()
	return "", nil
}

// fetchBranch fetches the adjust run's branch. A branch only this machine has (push was off, or failed) is fine; one
// that exists nowhere is a refusal. remote says whether origin has it.
func (r *Run) fetchBranch(ctx context.Context, name string) (remote bool, code string, err error) {
	ferr := r.main.Fetch(ctx, name)
	remote = ferr == nil && r.main.RemoteBranchExists(ctx, name)
	if !remote && !r.main.BranchExists(ctx, name) {
		if ferr != nil && !missingRemoteRef(ferr) {
			return false, codeFetchFailed, ferr
		}
		return false, codeBranchFailed, fmt.Errorf("the branch `%s` exists neither on this machine nor on origin; start over from %s", name, r.from)
	}
	if ferr != nil && !missingRemoteRef(ferr) {
		r.warn(fmt.Sprintf("Could not fetch `%s` from origin (%s); continuing from this machine's copy.", name, oneLine(ferr.Error(), 200)))
	}
	return remote, "", nil
}

// missingRemoteRef: git fetch says origin has no such branch.
func missingRemoteRef(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "couldn't find remote ref") || strings.Contains(msg, "could not find remote ref")
}

// catchUp brings the run's branch level with origin: a fast-forward when behind, nothing when ahead (the push sends
// the rest), a refusal when the two diverged (nothing ran: a person decides which side wins).
func (r *Run) catchUp(ctx context.Context, remote bool) (string, error) {
	if !remote {
		return "", nil
	}
	upstream := "refs/remotes/origin/" + r.branch
	rel, err := r.repo.Compare(ctx, "HEAD", upstream)
	if err != nil {
		return codeInternal, err
	}
	switch rel {
	case gitx.Behind:
		if err := r.repo.FastForward(ctx, upstream); err != nil {
			return codeBranchFailed, fmt.Errorf("could not fast-forward `%s` to origin: %w", r.branch, err)
		}
		r.info(fmt.Sprintf("Fast-forwarded `%s` to origin.", r.branch))
	case gitx.Ahead:
		r.info(fmt.Sprintf("`%s` has commits origin does not have yet; the push at the end sends them.", r.branch))
	case gitx.Diverged:
		return codeBranchDiverged, fmt.Errorf("the branch `%s` here and origin/%s diverged; reconcile them (or start over from %s) and run again", r.branch, r.branch, r.from)
	}
	return "", nil
}

// refuseTakenName: a fresh run never reuses a branch name, and never invents a `-2`.
func (r *Run) refuseTakenName(ctx context.Context, name string) (string, error) {
	if r.main.BranchExists(ctx, name) {
		return codeBranchFailed, fmt.Errorf("the branch `%s` already exists on this machine; nothing ran (continue it with Adjust, or delete it)", name)
	}
	onOrigin, err := r.main.RemoteHasBranch(ctx, name)
	if err != nil {
		onOrigin = r.main.RemoteBranchExists(ctx, name)
	}
	if onOrigin {
		return codeBranchFailed, fmt.Errorf("the branch `%s` already exists on origin; nothing ran (continue it with Adjust, or delete it)", name)
	}
	return "", nil
}

// emptyLeftover: the taken name is a branch an earlier run of this change created and left without a commit of its
// own (it failed before committing anything, so the server starts afresh): only on this machine, its tip already in
// origin/<default>. Reusing it loses nothing, refusing it would be a dead end.
func (r *Run) emptyLeftover(ctx context.Context, name string) bool {
	if !r.main.BranchExists(ctx, name) {
		return false
	}
	if onOrigin, err := r.main.RemoteHasBranch(ctx, name); err != nil || onOrigin {
		return false
	}
	empty, err := r.main.IsAncestor(ctx, "refs/heads/"+name, "refs/remotes/origin/"+r.from)
	return err == nil && empty
}

// reuseLeftover brings an empty leftover branch (checked out where the run works) to origin/<default>.
func (r *Run) reuseLeftover(ctx context.Context) (string, error) {
	if clean, err := r.repo.IsClean(ctx); err != nil || !clean {
		return codeBranchFailed, fmt.Errorf("the branch `%s` holds uncommitted work of an earlier run in %s; commit or remove it and run again", r.branch, r.repo.Dir)
	}
	if err := r.repo.FastForward(ctx, "refs/remotes/origin/"+r.from); err != nil {
		return codeBranchFailed, fmt.Errorf("could not bring `%s` to origin/%s: %w", r.branch, r.from, err)
	}
	r.info(fmt.Sprintf("Reusing `%s`: an earlier run left it without a commit.", r.branch))
	return "", nil
}

// clearPath makes room for a new worktree at path: an empty directory goes; a registered worktree or a directory
// with content stays, and the run is refused (nothing of anyone's is deleted here).
func (r *Run) clearPath(ctx context.Context, path string) (string, error) {
	if !gitx.Exists(path) {
		return "", nil
	}
	if list, err := r.main.WorktreeList(ctx); err == nil {
		for _, w := range list {
			if gitx.SamePath(w.Path, path) {
				what := "a detached HEAD"
				if w.Branch != "" {
					what = "`" + w.Branch + "`"
				}
				return codeWorktreeFailed, fmt.Errorf("the worktree at %s is on %s; remove it (`productos-agent worktrees prune`) and run again", path, what)
			}
		}
	}
	if err := os.Remove(path); err != nil {
		return codeWorktreeFailed, fmt.Errorf("%s is in the way and not empty; remove it and run again", path)
	}
	return "", nil
}

// saveLeftovers commits what an interrupted earlier run left uncommitted in the worktree, so the round starts clean
// and nothing is lost.
func (r *Run) saveLeftovers(ctx context.Context) error {
	clean, err := r.repo.IsClean(ctx)
	if err != nil || clean {
		return err
	}
	if _, err := r.repo.CommitWIP(ctx, r.commitMessage("WIP: ")); err != nil {
		return fmt.Errorf("the worktree at %s has uncommitted work that could not be committed: %w", r.repo.Dir, err)
	}
	r.info("Committed what an interrupted earlier run left in the worktree.")
	return nil
}

// touchMeta records the worktree's last use (prune removes worktrees nobody used for 14 days).
func (r *Run) touchMeta() {
	if r.worktree == "" || r.main == nil {
		return
	}
	m := worktree.Meta{Path: r.worktree, Branch: r.branch, RepositoryID: r.Start.Repository.ID, Repository: r.Start.Repository.FullName,
		Checkout: r.main.Dir, DefaultBranch: r.from, Subject: r.Start.Subject.Key, LastUsedAt: time.Now().UTC(), LastRunID: r.Start.RunID}
	if err := worktree.WriteMeta(m); err != nil {
		r.Log.Debug("worktree meta not written", "error", err)
	}
}

// preparedLine is the drawer's line once the run knows where it works.
func (r *Run) preparedLine() string {
	switch {
	case r.adjusting() && r.worktree != "":
		return fmt.Sprintf("Continuing `%s` at %s in the worktree %s.", r.branch, shortSHA(r.base), r.worktree)
	case r.adjusting():
		return fmt.Sprintf("Continuing `%s` at %s in %s.", r.branch, shortSHA(r.base), r.repo.Dir)
	case r.worktree != "":
		return fmt.Sprintf("Branch `%s` created from `origin/%s` in the worktree %s.", r.branch, r.from, r.worktree)
	}
	return fmt.Sprintf("Branch `%s` created from `origin/%s` in %s.", r.branch, r.from, r.repo.Dir)
}

// addWorktree is the part of prepareWorktree that touches the shared .git, under the repository lock: fetch, find
// where the branch is checked out, add or reuse the worktree, catch the branch up. created says the worktree is new.
func (r *Run) addWorktree(ctx context.Context, name, path string) (created bool, code string, err error) {
	main := r.main
	unlock := r.lockRepo()
	defer unlock()
	if err := main.Fetch(ctx, r.from); err != nil {
		return false, codeFetchFailed, err
	}
	_ = main.WorktreePrune(ctx) // forget worktrees whose directory is gone
	at, checkedOut, err := main.IsBranchCheckedOut(ctx, name)
	if err != nil {
		return false, codeWorktreeFailed, err
	}
	if checkedOut && gitx.SamePath(at, main.Dir) {
		return false, codeBranchCheckedOut, fmt.Errorf("the branch `%s` is checked out in your checkout at %s; switch it to another branch and run again", name, at)
	}
	ours := checkedOut && gitx.SamePath(at, path)
	if checkedOut && !ours {
		return false, codeBranchCheckedOut, fmt.Errorf("the branch `%s` is checked out in the worktree at %s; switch that one to another branch and run again", name, at)
	}

	created = false
	if r.adjusting() {
		remote, code, err := r.fetchBranch(ctx, name)
		if err != nil {
			return false, code, err
		}
		if !ours {
			if code, err := r.clearPath(ctx, path); err != nil {
				return false, code, err
			}
			if main.BranchExists(ctx, name) {
				err = main.WorktreeAdd(ctx, path, name)
			} else {
				err = main.WorktreeAddNew(ctx, path, name, "origin/"+name)
			}
			if err != nil {
				return false, codeWorktreeFailed, err
			}
			created = true
		}
		if r.repo, err = gitx.Open(path); err != nil {
			return false, codeWorktreeFailed, err
		}
		r.branch, r.worktree = name, r.repo.Dir
		if err := r.saveLeftovers(ctx); err != nil {
			return false, codeCommitFailed, err
		}
		if code, err := r.catchUp(ctx, remote); err != nil {
			return false, code, err
		}
	} else {
		code, err := r.refuseTakenName(ctx, name)
		leftover := err != nil && r.emptyLeftover(ctx, name)
		if err != nil && !leftover {
			return false, code, err
		}
		if !ours {
			if code, err := r.clearPath(ctx, path); err != nil {
				return false, code, err
			}
			if leftover {
				err = main.WorktreeAdd(ctx, path, name)
			} else {
				err = main.WorktreeAddNew(ctx, path, name, "origin/"+r.from)
			}
			if err != nil {
				return false, codeWorktreeFailed, err
			}
			created = true
		}
		if r.repo, err = gitx.Open(path); err != nil {
			return false, codeWorktreeFailed, err
		}
		r.branch, r.worktree = name, r.repo.Dir
		if leftover {
			if code, err := r.reuseLeftover(ctx); err != nil {
				return false, code, err
			}
		}
	}
	return created, "", nil
}
