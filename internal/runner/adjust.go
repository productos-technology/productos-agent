package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/protocol"
)

// An adjust run (`continue` on run.start) is a round on the change's branch and pull request: the agent reads what
// went wrong on GitHub (the failed CI jobs' log, the review comments since the previous run), hands the tool the brief,
// resumes the tool's session when this machine still has it, and updates the open pull request instead of opening one.

// ghPath is the signed-in gh the daemon detected, else whatever gh is on PATH (the pull request step's old rule).
func (r *Run) ghPath() string {
	if r.GH != "" {
		return r.GH
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	return ""
}

func (r *Run) ghRunner() ghExec {
	if r.ghExec != nil {
		return r.ghExec
	}
	if r.GH == "" {
		return nil
	}
	return ghCommand(r.GH, r.repo.Dir)
}

// readGitHub reads the CI log and the review comments of the open pull request an adjust run continues. Only with a
// signed-in gh; a failure is a warning, never the run's.
func (r *Run) readGitHub(ctx context.Context) (*ciLog, []reviewItem) {
	c := r.Start.Continue
	repo := r.Start.Repository.FullName
	if c == nil || !c.PR.Open() || repo == "" {
		return nil, nil
	}
	gh := r.ghRunner()
	if gh == nil {
		r.Log.Info("gh is not available: the brief goes without the CI log and the review comments")
		r.info("gh is not signed in on this machine: the CI log and the review comments are not read.")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	ci, err := readCI(ctx, gh, repo, c.PR.Number, r.branch, firstNonEmpty(c.HeadCommit, r.base))
	switch {
	case err != nil:
		r.Log.Warn("CI log not read", "error", err)
		r.warn("Could not read the CI of PR #" + strconv.Itoa(c.PR.Number) + ": " + oneLine(err.Error(), 300))
		ci = nil
	case len(ci.Jobs) == 0:
		r.info(fmt.Sprintf("Read the CI of PR #%d: no failed job on its head.", c.PR.Number))
	default:
		names := ci.Names()
		r.info(fmt.Sprintf("Read CI log: %d failed %s (%s).", len(names), plural(len(names), "job", "jobs"), oneLine(strings.Join(names, ", "), 400)))
		r.addNote(protocol.Note{Code: noteCIRead, Text: oneLine(strings.Join(names, ", "), maxReason)})
	}

	since := r.reviewsSince(ctx)
	reviews, err := readReviews(ctx, gh, repo, c.PR.Number, since)
	switch {
	case err != nil:
		r.Log.Warn("review comments not read", "error", err)
		r.warn("Could not read the review comments of PR #" + strconv.Itoa(c.PR.Number) + ": " + oneLine(err.Error(), 300))
	case len(reviews) == 0:
		r.info(fmt.Sprintf("Read the reviews of PR #%d: no new comment from the repository's people.", c.PR.Number))
	default:
		text := fmt.Sprintf("%d %s", len(reviews), plural(len(reviews), "comment", "comments"))
		r.info(fmt.Sprintf("Read %d review %s.", len(reviews), plural(len(reviews), "comment", "comments")))
		r.addNote(protocol.Note{Code: noteReviewsRead, Text: text})
	}
	return ci, reviews
}

// reviewsSince is when the previous run finished (when the server says), else the date of its head commit, else the
// beginning (every comment, the newest kept).
func (r *Run) reviewsSince(ctx context.Context) time.Time {
	c := r.Start.Continue
	if c.Previous != nil && c.Previous.FinishedAt != "" {
		if t, err := time.Parse(time.RFC3339, c.Previous.FinishedAt); err == nil {
			return t
		}
	}
	if c.HeadCommit != "" && r.repo != nil {
		if t, err := r.repo.CommitDate(ctx, c.HeadCommit); err == nil {
			return t
		}
	}
	return time.Time{}
}

// resumable is the session the first call resumes: the previous run's, on this device, with this tool.
func (r *Run) resumable() string {
	c := r.Start.Continue
	if c == nil || c.Session == nil || strings.TrimSpace(c.Session.ID) == "" {
		return ""
	}
	if c.Session.Tool != "" && c.Session.Tool != r.Start.Tool {
		return ""
	}
	return strings.TrimSpace(c.Session.ID)
}

var sessionNotFound = regexp.MustCompile(`(?i)no (?:conversation|session|thread|rollout)s? (?:was )?found|(?:conversation|session|thread|rollout)[^\n]{0,60}not found|could not find (?:the )?(?:conversation|session|thread|rollout)`)

// resumeFailed tells a resume that never started (the session is not on this machine any more, the tool refused it)
// from a session that ran and then failed: only the first falls back to a fresh session. Signed-out and limit
// failures would fail the fresh session too.
func resumeFailed(out toolOutcome, err error) (string, bool) {
	var re *runError
	switch {
	case errors.As(err, &re):
		if re.code == codeToolAuth || re.code == codeToolLimit || re.code == codeToolMissing || out.worked {
			return "", false
		}
		return re.msg, true
	case err != nil:
		return "", false
	case out.failed && !out.worked && out.failCode != codeToolAuth && out.failCode != codeToolLimit:
		return firstNonEmpty(firstLine(out.result), "the tool refused to resume"), true
	case out.failed && sessionNotFound.MatchString(out.result):
		return firstLine(out.result), true
	}
	return "", false
}

// supersede closes the pull request a "Start over from main" replaces, once this run's own pull request opened.
func (r *Run) supersede(ctx context.Context, newURL string) {
	old := r.Start.Options.SupersedesPR
	if old <= 0 || newURL == "" {
		return
	}
	newNumber := prNumber(newURL)
	if newNumber == old {
		return // the same pull request: nothing replaced it
	}
	ref := newURL
	if newNumber > 0 {
		ref = "#" + strconv.Itoa(newNumber)
	}
	gh := r.ghPath()
	if gh == "" {
		r.warn(fmt.Sprintf("Could not close PR #%d: gh is not installed.", old))
		r.addNote(protocol.Note{Code: notePRSupersedeFailed, Text: fmt.Sprintf("#%d stays open: gh is not installed", old)})
		return
	}
	args := []string{"pr", "close", strconv.Itoa(old), "--comment", "Superseded by " + ref}
	if repo := r.Start.Repository.FullName; repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := r.gh(ctx, gh, args...)
	if err != nil {
		why := oneLine(firstNonEmpty(firstLine(out), err.Error()), 200)
		r.warn(fmt.Sprintf("Could not close PR #%d as superseded: %s", old, why))
		r.addNote(protocol.Note{Code: notePRSupersedeFailed, Text: fmt.Sprintf("#%d stays open: %s", old, why)})
		return
	}
	r.info(fmt.Sprintf("Closed PR #%d as superseded by %s.", old, ref))
	r.addNote(protocol.Note{Code: notePRSuperseded, Text: fmt.Sprintf("#%d closed, superseded by %s", old, ref)})
}

var pullNumber = regexp.MustCompile(`/pull/(\d+)`)

func prNumber(url string) int {
	if m := pullNumber.FindStringSubmatch(url); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
