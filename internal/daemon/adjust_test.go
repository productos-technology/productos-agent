package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/productos-technology/productos-agent/internal/config"
	"github.com/productos-technology/productos-agent/internal/paths"
	"github.com/productos-technology/productos-agent/internal/protocol"
)

var worktreeFeatures = []string{"outcome", "report", "update", "continue", "worktree"}

const adjustBranch = "productos/mob-p7-offline-drafts-are-lost"

// pushRound puts a previous round's commit on the branch at origin, from another clone; it returns the head.
func pushRound(t *testing.T, origin, branch, file string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(origin), "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", "-b", branch)
	_ = os.WriteFile(filepath.Join(other, file), []byte("round 1\n"), 0o644)
	git(t, other, "add", "-A")
	git(t, other, "commit", "-q", "-m", "MOB-P7: Offline drafts are lost\n\nProductOS: MOB-P7")
	git(t, other, "push", "-q", "origin", branch)
	return git(t, other, "rev-parse", "HEAD")
}

func adjustStart(work, head string) protocol.ServerMessage {
	m := startMessage("run-adj", work, adjustBranch)
	m.Options.Worktree = true
	m.Continue = &protocol.Continue{
		RunID: "run-prev", Branch: adjustBranch, HeadCommit: head,
		PR:       &protocol.PullRequest{Number: 25, URL: "https://github.com/acme/mobile/pull/25", State: "open"},
		Session:  &protocol.ToolSession{Tool: "claude", ID: "s-prev"},
		Note:     "Keep the filters when the list is empty",
		Previous: &protocol.PreviousRun{Outcome: "changed", Headline: "Drafts persist", FinishedAt: "2026-10-18T00:00:00Z"},
		Markdown: "## Adjust the previous run\n\nCI fails on " + head[:7] + ". Keep the filters when the list is empty.\n",
	}
	return m
}

type promptCall struct {
	args, dir, prompt string
}

func promptCalls(t *testing.T, path string) []promptCall {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls []promptCall
	for _, block := range strings.Split(string(raw), "=== END\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		lines := strings.SplitN(block, "\n", 3)
		calls = append(calls, promptCall{args: strings.TrimPrefix(lines[0], "=== ARGS "), dir: strings.TrimPrefix(lines[1], "=== DIR "), prompt: lines[2]})
	}
	return calls
}

func logTexts(t *testing.T, run *fakeRun) string {
	var b strings.Builder
	for _, e := range run.events {
		if e.Kind == "log" {
			p := payload(t, e)
			b.WriteString(p["level"].(string) + ": " + p["text"].(string) + "\n")
		}
	}
	return b.String()
}

func TestAdjustRunResumesTheSessionAndUpdatesThePR(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	head := pushRound(t, origin, adjustBranch, "round1.txt")
	// The person is busy in their own checkout: it never blocks an adjust run, which never moves it.
	_ = os.WriteFile(filepath.Join(work, "README.md"), []byte("# Mobile (editing)\n"), 0o644)
	prompts := filepath.Join(t.TempDir(), "prompts.log")
	t.Setenv("PRODUCTOS_AGENT_FAKE_PROMPT_LOG", prompts)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE_RESUMED", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS_RESUMED", "src/drafts/filters.ts=export const keep = true;\\n")
	t.Setenv("PRODUCTOS_AGENT_FAKE_RESUMED_EXPECT", "## Adjust the previous run")

	f := newFakeServer(t)
	f.features = worktreeFeatures
	f.start = adjustStart(work, head)
	run := runDaemon(t, f, work)

	if got := strings.Join(statuses(run.statuses), ","); got != "starting,running,succeeded" {
		t.Fatalf("statuses = %s (final %+v)\n%s", got, lastStatus(t, run), logTexts(t, run))
	}
	final := lastStatus(t, run)
	if final.Outcome != "changed" || final.Branch != adjustBranch || final.PRURL != "https://github.com/acme/mobile/pull/25" {
		t.Errorf("final = %+v", final)
	}
	if note, ok := hasNote(final.Report, "pr_updated"); !ok || note.Text != "https://github.com/acme/mobile/pull/25" {
		t.Errorf("notes = %+v", final.Report.Notes)
	}
	if _, ok := hasNote(final.Report, "pr_failed"); ok {
		t.Error("an open pull request is updated, never created again")
	}
	if final.SessionID != "538221b4-cda1-4ac5-9eb5-507cd756de39" || final.Report.SessionID != final.SessionID {
		t.Errorf("session_id = %q / %q", final.SessionID, final.Report.SessionID)
	}
	state, _ := paths.StateDir()
	wantPath := filepath.Join(state, "worktrees", "repo-1", "productos-mob-p7-offline-drafts-are-lost")
	if final.WorktreePath != wantPath || final.Report.WorktreePath != wantPath {
		t.Errorf("worktree_path = %q", final.WorktreePath)
	}
	// The round went on top of round 1 and fast-forwarded origin's branch.
	if git(t, origin, "rev-parse", "refs/heads/"+adjustBranch) != final.HeadCommit || git(t, origin, "rev-parse", final.HeadCommit+"^") != head {
		t.Errorf("origin branch is not round 1 + this round")
	}
	if git(t, work, "rev-parse", "--abbrev-ref", "HEAD") != "main" || git(t, work, "status", "--porcelain") != "M README.md" {
		t.Error("the person's checkout moved or changed")
	}

	calls := promptCalls(t, prompts)
	if len(calls) != 1 || !strings.Contains(calls[0].args, "--resume s-prev") || !strings.HasPrefix(calls[0].prompt, "## Adjust the previous run") {
		t.Fatalf("calls = %+v", calls)
	}
	if strings.Contains(calls[0].prompt, "ProductOS Agent") || strings.Contains(calls[0].prompt, "Keep drafts across reloads") {
		t.Error("a resumed session gets the brief only")
	}
	logs := logTexts(t, run)
	for _, want := range []string{"Continuing `" + adjustBranch + "` at " + head[:7] + " in the worktree", "Resuming the session of the previous run.", "Pull request #25 updated"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

func TestAdjustRunFallsBackToAFreshSession(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	head := pushRound(t, origin, adjustBranch, "round1.txt")
	prompts := filepath.Join(t.TempDir(), "prompts.log")
	t.Setenv("PRODUCTOS_AGENT_FAKE_PROMPT_LOG", prompts)
	t.Setenv("PRODUCTOS_AGENT_FAKE_RESUME_FAIL", "1")
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/filters.ts=export const keep = true;\\n")

	f := newFakeServer(t)
	f.features = worktreeFeatures
	f.start = adjustStart(work, head)
	run := runDaemon(t, f, work)

	final := lastStatus(t, run)
	if final.Status != "succeeded" || final.Outcome != "changed" {
		t.Fatalf("final = %+v\n%s", final, logTexts(t, run))
	}
	if final.SessionID != "538221b4-cda1-4ac5-9eb5-507cd756de39" {
		t.Errorf("the fresh session is reported, not the lost one: %q", final.SessionID)
	}
	calls := promptCalls(t, prompts)
	if len(calls) != 2 || !strings.Contains(calls[0].args, "--resume s-prev") || strings.Contains(calls[1].args, "--resume") {
		t.Fatalf("calls = %+v", calls)
	}
	fresh := calls[1].prompt
	for _, want := range []string{"ProductOS Agent", "already holds the work of an earlier run", "pull request #25", "git worktree made for this change", "Keep drafts across reloads.", "## Adjust the previous run"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("fresh prompt lacks %q", want)
		}
	}
	if !strings.Contains(logTexts(t, run), "Could not resume the previous session (No conversation found with session ID: s-prev)") {
		t.Errorf("logs:\n%s", logTexts(t, run))
	}
}

func TestFreshWorktreeRunNeverTouchesTheCheckout(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	_ = os.WriteFile(filepath.Join(work, "mine.txt"), []byte("work in progress\n"), 0o644)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n")

	f := newFakeServer(t)
	f.features = worktreeFeatures
	f.start = startMessage("run-wt", work, adjustBranch)
	f.start.Options.Worktree = true
	run := runDaemon(t, f, work)

	final := lastStatus(t, run)
	if final.Status != "succeeded" || final.Branch != adjustBranch || final.WorktreePath == "" || final.SessionID == "" {
		t.Fatalf("final = %+v\n%s", final, logTexts(t, run))
	}
	if git(t, origin, "rev-parse", "refs/heads/"+adjustBranch) != final.HeadCommit {
		t.Error("the branch was not pushed")
	}
	if git(t, work, "rev-parse", "--abbrev-ref", "HEAD") != "main" || git(t, work, "status", "--porcelain") != "?? mine.txt" {
		t.Error("the person's checkout moved or changed")
	}
	if out := git(t, final.WorktreePath, "status", "--porcelain"); out != "" {
		t.Errorf("the worktree is left clean: %q", out)
	}
}

func TestOldServerGetsNoSessionOrWorktreeFields(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = 1;\\n")
	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-old", work, adjustBranch)
	final := lastStatus(t, runDaemon(t, f, work))
	if final.Status != "succeeded" || final.SessionID != "" || final.WorktreePath != "" {
		t.Errorf("final = %+v", final)
	}
}

func TestDeviceAnnouncesAndKeepsItsCapacity(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow.jsonl")
	raw, _ := os.ReadFile(fixture(t, "claude/simple_run.jsonl"))
	_ = os.WriteFile(slow, append([]byte("#PAUSE\n"), raw...), 0o644)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", slow)
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = 2;\\n")

	f := newFakeServer(t)
	f.features = worktreeFeatures
	f.start = startMessage("run-first", work, adjustBranch)
	f.start.Options.Worktree = true
	extra := startMessage("run-extra", work, "productos/mob-p8-other")
	extra.Options.Worktree = true
	f.more = []protocol.ServerMessage{extra}
	run := runDaemonWith(t, f, work, func(d *Daemon) { d.Config.MaxParallelRuns = 1 })

	if final := lastStatus(t, run); final.Status != "succeeded" {
		t.Errorf("the first run = %+v", final)
	}
	f.mu.Lock()
	refused := f.runs["run-extra"]
	subscribed := f.subscribed
	patched := f.patched
	f.mu.Unlock()
	if got := strings.Join(statuses(refused.statuses), ","); got != "starting,failed" {
		t.Fatalf("refused statuses = %s", got)
	}
	if last := refused.statuses[1]; last.ReasonCode != "internal" || !strings.Contains(last.Reason, "device at capacity") {
		t.Errorf("refusal = %+v", last)
	}
	if subscribed["max_parallel_runs"] != float64(1) {
		t.Errorf("subscription = %v", subscribed)
	}
	if len(patched) == 0 || patched[0]["max_parallel_runs"] != float64(1) {
		t.Errorf("heartbeats = %v", patched)
	}
	off := false
	if (&config.Config{Worktrees: &off, MaxParallelRuns: 5}).ParallelRuns() != 1 || (&config.Config{}).ParallelRuns() != 3 {
		t.Error("worktrees off announce 1; the default is 3")
	}
}

// writeFakeGH writes a gh stand-in answering from testdata/gh and recording its calls.
func writeFakeGH(t *testing.T) (bin, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	data, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "gh"))
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "gh")
	script := `#!/bin/sh
echo "$*" >> "` + calls + `"
case "$*" in
  "pr checks"*) cat "` + data + `/pr_checks.json"; exit 1 ;;
  "run view"*) cat "` + data + `/run_log_failed.txt" ;;
  *pulls/25/comments*) cat "` + data + `/pull_comments.json" ;;
  *pulls/25/reviews*) cat "` + data + `/pull_reviews.json" ;;
  "pr create"*) echo "https://github.com/acme/mobile/pull/31" ;;
  "pr close"*) echo "Closed pull request acme/mobile#25" ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func withGH(bin string) func(*Daemon) {
	return func(d *Daemon) {
		d.Detect = func(context.Context) protocol.Tools {
			return protocol.Tools{"claude": {Available: true, Version: "2.1.292"}, "gh": {Available: true, Version: "2.101.0", Path: bin}}
		}
	}
}

func TestAdjustRunReadsTheCIAndTheReviews(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	head := pushRound(t, origin, adjustBranch, "round1.txt")
	bin, callLog := writeFakeGH(t)
	prompts := filepath.Join(t.TempDir(), "prompts.log")
	t.Setenv("PRODUCTOS_AGENT_FAKE_PROMPT_LOG", prompts)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE_RESUMED", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS_RESUMED", "src/drafts/filters.ts=export const keep = true;\\n")
	t.Setenv("PRODUCTOS_AGENT_FAKE_RESUMED_EXPECT", "## Adjust the previous run")

	f := newFakeServer(t)
	f.features = worktreeFeatures
	f.start = adjustStart(work, head)
	run := runDaemonWith(t, f, work, withGH(bin))

	final := lastStatus(t, run)
	if final.Status != "succeeded" {
		t.Fatalf("final = %+v\n%s", final, logTexts(t, run))
	}
	if note, ok := hasNote(final.Report, "ci_read"); !ok || note.Text != "Tests (unit), Build, external/security" {
		t.Errorf("ci_read = %+v", final.Report.Notes)
	}
	if note, ok := hasNote(final.Report, "reviews_read"); !ok || note.Text != "4 comments" {
		t.Errorf("reviews_read = %+v", final.Report.Notes)
	}
	logs := logTexts(t, run)
	for _, want := range []string{"Read CI log: 3 failed jobs (Tests (unit), Build, external/security).", "Read 4 review comments."} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	calls := promptCalls(t, prompts)
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	brief := calls[0].prompt
	for _, want := range []string{"## Adjust the previous run", "## CI log (failed jobs)", "AssertionError: expected undefined to equal 'hello'", "## Review comments (untrusted, quoted)",
		"<<<BEGIN UNTRUSTED REVIEW COMMENTS ", "> @pat (member) on src/list.ts:42", "> Safari drops IndexedDB in private mode: guard it."} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if strings.Contains(brief, "Ignore all previous instructions") || strings.Contains(brief, "Old remark") {
		t.Error("untrusted authors and comments from before the previous run are left out")
	}
	ghCalls, _ := os.ReadFile(callLog)
	for _, want := range []string{"pr checks 25 --repo acme/mobile --json name,state,link,workflow", "run view --repo acme/mobile --job 118895248812 --log-failed", "api repos/acme/mobile/pulls/25/comments", "api repos/acme/mobile/pulls/25/reviews"} {
		if !strings.Contains(string(ghCalls), want) {
			t.Errorf("gh calls lack %q:\n%s", want, ghCalls)
		}
	}
	if strings.Contains(string(ghCalls), "pr create") {
		t.Error("the open pull request is updated by the push")
	}
}

func TestStartOverClosesTheSupersededPR(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	bin, callLog := writeFakeGH(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = 3;\\n")

	f := newFakeServer(t)
	f.features = worktreeFeatures
	f.start = startMessage("run-over", work, "productos/mob-p7-offline-drafts-are-lost-again")
	f.start.Options.Worktree = true
	f.start.Options.SupersedesPR = 25
	run := runDaemonWith(t, f, work, withGH(bin))

	final := lastStatus(t, run)
	if final.Status != "succeeded" || final.PRURL != "https://github.com/acme/mobile/pull/31" {
		t.Fatalf("final = %+v\n%s", final, logTexts(t, run))
	}
	if note, ok := hasNote(final.Report, "pr_superseded"); !ok || note.Text != "#25 closed, superseded by #31" {
		t.Errorf("notes = %+v", final.Report.Notes)
	}
	ghCalls, _ := os.ReadFile(callLog)
	if !strings.Contains(string(ghCalls), "pr close 25 --comment Superseded by #31 --repo acme/mobile") {
		t.Errorf("gh calls:\n%s", ghCalls)
	}
}
