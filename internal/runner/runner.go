// Package runner executes one `run.start`: prepares where the run works (the change's own worktree, or the linked
// checkout; a new branch, or for an adjust run the branch it continues), drives the tool in its JSON streaming mode
// (resuming the previous session when this machine has it), turns its stream into protocol events, relays questions
// and answers, and finishes with an outcome git decides (a commit, a push, a pull request opened or updated; or
// nothing to change), a structured report and the final status. Work the tool did is never lost: a run that fails or
// is cancelled commits it on its branch.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/productos-technology/productos-agent/internal/config"
	"github.com/productos-technology/productos-agent/internal/gitx"
	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/stream"
	"github.com/productos-technology/productos-agent/internal/tools"
	"github.com/productos-technology/productos-agent/internal/version"
	"github.com/productos-technology/productos-agent/internal/worktree"
)

const (
	// RunTimeout stays under AgentRun::STUCK_AFTER (2 h): the agent's own `timeout` must win the race with the
	// server's stuck sweeper, so the person reads what happened and the work is preserved.
	RunTimeout   = 110 * time.Minute
	maxQuestions = 10
	maxOutput    = 1000
	maxSummary   = 20000
	// maxReason: `reason` is one line, the first line of the tool's last message at most.
	maxReason = 280
	// wrapUp bounds what happens after the tool: preserving work, pushing, the final status (even after a cancel).
	wrapUp = 3 * time.Minute
)

// Answer is a `run.answer`.
type Answer struct {
	Seq  int
	Text string
}

// Run is one execution.
type Run struct {
	Start    protocol.RunStart
	Reporter Reporter
	Config   *config.Config
	Log      *slog.Logger
	// ToolPath overrides tool detection (tests); ToolVersion is the detected version, for report.tool.
	ToolPath    string
	ToolVersion string
	// Timeout is overridable for tests.
	Timeout time.Duration
	// Supports asks whether the server's hello announced a feature; without "outcome" a no_change goes out as the old
	// failure. Asked when the run ends: a handed-over run starts before the hello arrives.
	Supports func(feature string) bool
	// BeforePrepare runs once the run is acknowledged and before the checkout is touched (the daemon's self-update;
	// it does not return when the process re-executes itself). The notes it returns go to the report.
	BeforePrepare func(ctx context.Context, run *Run) []protocol.Note
	// Notes are facts known before the run started (the daemon's `updated`).
	Notes []protocol.Note
	// GH is the signed-in gh the daemon detected ("" without): an adjust run reads the CI log and the review comments
	// with it.
	GH string
	// Coord serialises the git work of the device's runs per repository and keeps two runs out of one directory (the
	// daemon's; a private one when nil).
	Coord *worktree.Coordinator

	ghExec ghExec // tests

	mu        sync.Mutex
	cancelled bool
	reason    string
	cancelCtx context.CancelFunc
	procPID   int
	answers   chan Answer

	emitter *emitter
	// main is the person's checkout; repo is where the run works (main itself, or the change's worktree).
	main     *gitx.Repo
	repo     *gitx.Repo
	worktree string
	release  func()
	from     string
	branch   string
	base     string
	// sessionID is the session the next call resumes; lastSession the tool's session as it last announced it (what
	// the final status reports, for the next adjust run).
	sessionID   string
	lastSession string
	reported    map[string]gitx.Change
	tests       testStats
	prior       []string
	prose       string
	answer      *toolAnswer
	notes       []protocol.Note
}

type testStats struct {
	passed, failed int
	// lastFailed is the command of the last test run when it failed (tests_failing).
	lastFailed string
}

// New prepares a Run; Execute does the work.
func New(start protocol.RunStart, reporter Reporter, cfg *config.Config, log *slog.Logger) *Run {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &Run{Start: start, Reporter: reporter, Config: cfg, Log: log.With("run_id", start.RunID), Timeout: RunTimeout, answers: make(chan Answer, 4), reported: map[string]gitx.Change{}}
}

// Cancel stops the tool; Execute then reports `cancelled`.
func (r *Run) Cancel(reason string) {
	r.mu.Lock()
	r.cancelled = true
	r.reason = reason
	pid := r.procPID
	cancel := r.cancelCtx
	r.mu.Unlock()
	if pid > 0 {
		killProcessGroup(pid)
	}
	if cancel != nil {
		cancel()
	}
}

// Answer delivers a `run.answer`.
func (r *Run) Answer(a Answer) {
	select {
	case r.answers <- a:
	default:
		r.Log.Warn("answer dropped: nobody waiting")
	}
}

// Emit queues an event on the run's stream (the daemon's update log lines).
func (r *Run) Emit(ev protocol.Event) { r.emitter.Emit(ev) }

// Flush waits until every queued event landed on the server.
func (r *Run) Flush(ctx context.Context) error {
	_, err := r.emitter.Flush(ctx)
	return err
}

func (r *Run) isCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

var reasonCodeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

// Execute runs everything and returns the final report it sent (or tried to send).
func (r *Run) Execute(parent context.Context) protocol.StatusReport {
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	r.mu.Lock()
	r.cancelCtx = cancel
	r.mu.Unlock()
	for _, n := range r.Notes {
		r.addNote(n)
	}

	r.emitter = newEmitter(r.Start.RunID, r.Reporter, r.Log)
	final := r.execute(ctx)
	if r.isCancelled() {
		r.mu.Lock()
		reason := r.reason
		r.mu.Unlock()
		final = protocol.StatusReport{Status: protocol.StatusCancelled}
		if reasonCodeRE.MatchString(reason) {
			final.ReasonCode = reason
		}
	}
	// The run's own context may be over (cancel, timeout): what follows gets its own time.
	wrap, wrapCancel := context.WithTimeout(context.Background(), wrapUp)
	defer wrapCancel()
	if final.Status != protocol.StatusSucceeded {
		r.preserve(wrap, &final)
	}
	if r.tests.lastFailed != "" {
		r.addNote(protocol.Note{Code: noteTestsFailing, Text: "the last test run failed: " + oneLine(r.tests.lastFailed, 200)})
	}
	final.Summary = clip(r.summary(final), maxSummary)
	final.Report = r.report()
	if r.lastSession != "" && r.supports("continue") {
		final.SessionID = r.lastSession
		final.Report.SessionID = r.lastSession
	}
	if r.worktree != "" && r.supports("worktree") {
		final.WorktreePath = r.worktree
		final.Report.WorktreePath = r.worktree
	}
	r.touchMeta()
	if r.release != nil {
		r.release()
	}
	r.emitter.Close(context.Background())
	if err := r.Reporter.Status(wrap, r.Start.RunID, final); err != nil && !errors.Is(err, ErrRunFinished) {
		r.Log.Error("final status not delivered", "status", final.Status, "error", err)
	}
	return final
}

func (r *Run) execute(ctx context.Context) protocol.StatusReport {
	if !r.Start.Resumed { // a handed-over run was acknowledged by the previous process
		if err := r.status(ctx, protocol.StatusStarting); err != nil {
			return r.fail(serverCode(err), "could not acknowledge the run: "+err.Error())
		}
	}
	driver, ok := DriverFor(r.Start.Tool)
	if !ok {
		return r.fail(codeInternal, "unknown tool "+r.Start.Tool)
	}
	bin := r.ToolPath
	if bin == "" {
		path, found := tools.Path(r.Start.Tool)
		if !found {
			return r.fail(codeToolMissing, fmt.Sprintf("%s is not installed on this machine", r.Start.Tool))
		}
		bin = path
	}
	if r.BeforePrepare != nil && !r.Start.Resumed {
		for _, n := range r.BeforePrepare(ctx, r) {
			r.addNote(n)
		}
	}
	if code, err := r.prepare(ctx); err != nil {
		return r.fail(code, err.Error())
	}
	if err := r.status(ctx, protocol.StatusRunning); err != nil {
		return r.fail(serverCode(err), "could not report the run as running: "+err.Error())
	}
	r.info(r.preparedLine())
	r.lookupPriorCommits(ctx)

	inv := Invocation{Bin: bin, Dir: r.repo.Dir, Config: r.Config}
	if driver.SchemaFile() {
		path, err := writeSchemaFile()
		if err != nil {
			return r.fail(codeInternal, "could not write the report schema: "+err.Error())
		}
		defer os.Remove(path)
		inv.SchemaFile = path
	}

	facts := promptFacts{Dir: r.repo.Dir, Branch: r.branch, AsksWithTool: driver.AsksWithTool(), Prior: r.prior, Worktree: r.worktree != ""}
	if c := r.Start.Continue; c != nil {
		ci, reviews := r.readGitHub(ctx)
		facts.Adjust, facts.Brief = c, renderBrief(c, ci, reviews, "")
	}
	input := buildPrompt(r.Start, facts)
	resuming := false
	if id := r.resumable(); id != "" {
		// The previous run's session on this machine: it knows the change already, so it gets the brief only.
		r.sessionID, resuming, input = id, true, facts.Brief
		r.info("Resuming the session of the previous run.")
	}
	for round := 0; ; round++ {
		if stop, ok := r.interrupted(ctx); ok {
			return stop
		}
		out, err := r.runTool(ctx, driver, inv, input)
		if stop, ok := r.interrupted(ctx); ok {
			return stop
		}
		if resuming {
			resuming = false
			if why, failed := resumeFailed(out, err); failed {
				r.warn("Could not resume the previous session (" + oneLine(why, 200) + "); starting a new one with the change definition and the brief.")
				r.sessionID, r.lastSession = "", ""
				input = buildPrompt(r.Start, facts)
				out, err = r.runTool(ctx, driver, inv, input)
				if stop, ok := r.interrupted(ctx); ok {
					return stop
				}
			}
		}
		if err != nil {
			var re *runError
			if errors.As(err, &re) {
				return r.fail(re.code, re.msg)
			}
			return r.fail(codeInternal, err.Error())
		}
		r.answer = extractReport(out.items)
		if out.failed {
			// The result is the error; the summary keeps what the tool last said.
			r.prose = firstNonEmpty(strings.TrimSpace(out.prose), r.prose)
			return r.fail(out.failCode, firstNonEmpty(firstLine(out.result), r.Start.Tool+" ended with an error"))
		}
		r.prose = firstNonEmpty(strings.TrimSpace(out.result), strings.TrimSpace(out.prose), r.prose)
		question := out.question
		if question == nil && r.answer != nil && r.answer.Claim == claimNeedsInput && r.answer.Question != "" {
			question = &stream.Item{Kind: stream.Question, Text: r.answer.Question, Options: r.answer.Options}
		}
		if question == nil && r.answer == nil && !out.changed && looksLikeQuestion(out.result) {
			question = &stream.Item{Kind: stream.Question, Text: out.result}
		}
		if question == nil {
			break
		}
		if round+1 >= maxQuestions {
			return r.fail(codeTooManyQuestions, fmt.Sprintf("the tool asked more than %d questions", maxQuestions-1))
		}
		if out.sessionID == "" {
			return r.fail(codeNoSession, "the tool asked a question but gave no session to resume")
		}
		answer, err := r.ask(ctx, question)
		if err != nil {
			if stop, ok := r.interrupted(ctx); ok {
				return stop
			}
			return r.fail(serverCode(err), err.Error())
		}
		r.sessionID = out.sessionID
		input = answerPrompt(question.Text, answer.Text)
	}
	return r.finish(ctx)
}

// interrupted reports why the run must stop now, if it must: cancelled, out of time, the agent stopping, or the
// server refusing the run.
func (r *Run) interrupted(ctx context.Context) (protocol.StatusReport, bool) {
	switch {
	case r.isCancelled():
		return protocol.StatusReport{Status: protocol.StatusCancelled}, true
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return r.fail(codeTimeout, fmt.Sprintf("the run exceeded the agent's limit of %s", r.Timeout)), true
	case ctx.Err() != nil:
		return r.fail(codeInternal, "the agent stopped while the run was in progress"), true
	}
	if dead := r.emitter.Dead(); dead != nil {
		return r.fail(codeServerClosed, "the server closed the run: "+dead.Error()), true
	}
	return protocol.StatusReport{}, false
}

// serverCode: a refusal from the server (the run is over there) is server_closed; anything else is internal.
func serverCode(err error) string {
	if errors.Is(err, ErrRunFinished) {
		return codeServerClosed
	}
	return codeInternal
}

// lookupPriorCommits finds the commits on origin/<default> that already carry the trailer: an earlier run or someone
// by hand may have done (part of) the work. The prompt lists them and the report keeps them.
func (r *Run) lookupPriorCommits(ctx context.Context) {
	trailer := strings.TrimSpace(r.Start.Options.CommitTrailer)
	if trailer == "" {
		return
	}
	commits, err := r.repo.CommitsWithTrailer(ctx, "origin/"+r.from, trailer, 10)
	if err != nil {
		r.Log.Debug("prior commits", "error", err)
		return
	}
	r.prior = commits
	if len(commits) > 0 {
		text := fmt.Sprintf("%d earlier commit(s) on origin/%s carry %s", len(commits), r.from, trailer)
		r.addNote(protocol.Note{Code: notePriorCommits, Text: text})
		r.info(text + ".")
	}
}

func writeSchemaFile() (string, error) {
	f, err := os.CreateTemp("", "productos-report-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(reportSchemaSource); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

type toolOutcome struct {
	result    string // the result's text: the tool's final prose
	prose     string // the last assistant message
	failed    bool
	failCode  string
	question  *stream.Item
	sessionID string
	changed   bool
	// worked: the session did something (a message, a tool call, a question), so it really started.
	worked bool
	items  []stream.Item // messages and the result, for extractReport
}

// runError carries the reason code of a tool session that went wrong.
type runError struct{ code, msg string }

func (e *runError) Error() string { return e.msg }

// runTool runs one tool session (initial or resumed) to its end, streaming events as it goes.
func (r *Run) runTool(ctx context.Context, driver Driver, inv Invocation, input string) (toolOutcome, error) {
	inv.SessionID = r.sessionID
	cmd := driver.Command(ctx, inv)
	setProcessGroup(cmd)
	// On timeout, take the whole group down (the tool's shells and test runners too), not only the tool.
	cmd.Cancel = func() error {
		killProcessGroup(cmd.Process.Pid)
		return nil
	}
	cmd.Stdin = strings.NewReader(input)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return toolOutcome{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, max: 64 << 10}
	r.Log.Info("starting tool", "tool", driver.Name(), "resume", r.sessionID != "")
	if err := cmd.Start(); err != nil {
		code := codeToolCrashed
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			code = codeToolMissing
		}
		return toolOutcome{}, &runError{code, fmt.Sprintf("could not start %s: %v", driver.Name(), err)}
	}
	r.mu.Lock()
	r.procPID = cmd.Process.Pid
	r.mu.Unlock()

	parser := driver.NewParser()
	out := toolOutcome{sessionID: r.sessionID}
	pendingUses := map[string]stream.Item{}
	sawResult := false

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 32<<20)
	for scanner.Scan() {
		for _, item := range parser.Feed(scanner.Text()) {
			r.handle(ctx, item, &out, pendingUses, &sawResult)
		}
	}
	for _, item := range parser.Flush() {
		r.handle(ctx, item, &out, pendingUses, &sawResult)
	}
	waitErr := cmd.Wait()
	r.mu.Lock()
	r.procPID = 0
	r.mu.Unlock()
	r.refreshChanges(ctx, &out)
	if out.sessionID != "" && out.sessionID != inv.SessionID || out.worked {
		r.lastSession = out.sessionID
	}

	if r.isCancelled() {
		return out, nil
	}
	if waitErr != nil && !sawResult {
		why := lastLine(stderr.String())
		if why == "" {
			why = waitErr.Error()
		}
		return out, &runError{crashCode(stderr.String()), fmt.Sprintf("%s exited (%v): %s", driver.Name(), waitErr, why)}
	}
	if !sawResult && !out.failed && out.result == "" {
		out.result = firstNonEmpty(out.prose, "the tool ended without a final message")
	}
	return out, nil
}

func (r *Run) handle(ctx context.Context, item stream.Item, out *toolOutcome, pending map[string]stream.Item, sawResult *bool) {
	switch item.Kind {
	case stream.Session:
		out.sessionID = item.SessionID
	case stream.Message:
		out.worked = true
		out.prose = item.Text
		out.items = append(out.items, item)
		r.emitter.Emit(protocol.Message("assistant", clip(item.Text, 20000)))
	case stream.ToolUse:
		out.worked = true
		pending[item.ID] = item
	case stream.ToolResult:
		out.worked = true
		use, known := pending[item.ID]
		delete(pending, item.ID)
		if !known {
			use = item
		}
		name := firstNonEmpty(item.Name, use.Name, "tool")
		command := firstNonEmpty(item.Command, use.Command)
		var duration int64
		if known && !use.At.IsZero() && !item.At.IsZero() {
			duration = item.At.Sub(use.At).Milliseconds()
		}
		if command != "" && stream.IsTestCommand(command) {
			passed := !item.IsError
			if passed {
				r.tests.passed++
				r.tests.lastFailed = ""
			} else {
				r.tests.failed++
				r.tests.lastFailed = command
			}
			r.emitter.Emit(protocol.Event{Kind: protocol.KindTest, Payload: protocol.TestPayload{Command: clip(command, 500), Passed: passed, Summary: tail(item.Output, 500), DurationMS: duration}})
			return
		}
		input := use.Input
		if input == nil && command != "" {
			input = map[string]any{"command": command}
		}
		r.emitter.Emit(protocol.Event{Kind: protocol.KindToolUse, Payload: protocol.ToolUsePayload{Name: name, Input: trimInput(input), Output: tail(item.Output, maxOutput), DurationMS: duration}})
	case stream.FileChange:
		r.refreshChanges(ctx, out)
	case stream.Question:
		out.worked = true
		q := item
		out.question = &q
	case stream.Result:
		*sawResult = true
		out.result = item.Text
		out.failed = item.IsFailure
		if item.IsFailure {
			out.failCode = toolFailureCode(item.Subtype, item.ErrorStatus, item.Text)
		}
		out.items = append(out.items, item)
		for _, d := range item.Denials {
			r.addNote(denialNote(d))
		}
	case stream.Log:
		if item.Level == "warn" || item.Level == "error" {
			r.emitter.Emit(protocol.Log(item.Level, clip(item.Text, 2000)))
		} else {
			r.Log.Debug("tool line", "text", clip(item.Text, 300))
		}
	}
}

// refreshChanges diffs the checkout against the base commit and emits file_change for what moved.
func (r *Run) refreshChanges(ctx context.Context, out *toolOutcome) {
	if r.repo == nil || r.base == "" {
		return
	}
	changes, err := r.repo.Changes(ctx, r.base)
	if err != nil {
		r.Log.Debug("diff failed", "error", err)
		return
	}
	seen := map[string]bool{}
	for _, c := range changes {
		seen[c.Path] = true
		prev, had := r.reported[c.Path]
		if had && prev == c {
			continue
		}
		r.reported[c.Path] = c
		out.changed = true
		payload := protocol.FileChangePayload{Path: c.Path, Action: c.Action, From: c.From}
		if !c.Binary {
			a, d := c.Additions, c.Deletions
			payload.Additions, payload.Deletions = &a, &d
		}
		r.emitter.Emit(protocol.Event{Kind: protocol.KindFileChange, Payload: payload})
	}
	for path := range r.reported {
		if !seen[path] {
			delete(r.reported, path)
		}
	}
	if len(r.reported) > 0 {
		out.changed = true
	}
}

// ask emits the question, waits for the answer (the server moves the run to waiting_input by itself).
func (r *Run) ask(ctx context.Context, q *stream.Item) (Answer, error) {
	r.emitter.Emit(protocol.Event{Kind: protocol.KindQuestion, Payload: protocol.QuestionPayload{Text: clip(q.Text, 8000), Options: q.Options}})
	seq, err := r.emitter.Flush(ctx)
	if err != nil {
		return Answer{}, fmt.Errorf("could not deliver the question: %w", err)
	}
	r.Log.Info("waiting for an answer", "question_seq", seq)
	for {
		select {
		case <-ctx.Done():
			return Answer{}, errors.New("the run ended while waiting for an answer")
		case a := <-r.answers:
			if a.Seq != 0 && seq != 0 && a.Seq != seq {
				r.Log.Warn("answer for another question", "got", a.Seq, "want", seq)
				continue
			}
			r.emitter.Emit(protocol.Message("user", clip(a.Text, 8000)))
			return a, nil
		}
	}
}

// finish commits what the tool left and lets git decide the outcome (reconcile); on `changed` it pushes and opens
// the pull request.
func (r *Run) finish(ctx context.Context) protocol.StatusReport {
	message := r.commitMessage("")
	committed, err := r.repo.CommitAll(ctx, message)
	if err != nil {
		return r.fail(codeCommitFailed, "commit failed: "+err.Error())
	}
	if committed {
		r.info("Committed the remaining changes.")
	}
	head, err := r.repo.Head(ctx)
	if err != nil {
		return r.fail(codeInternal, err.Error())
	}
	v := reconcile(r.answer, gitFacts{Base: r.base, Head: head})
	switch {
	case v.Status == protocol.StatusFailed:
		reason := v.Reason
		if v.ReasonCode == codeNoChanges && r.answer == nil {
			if line := firstLine(r.prose); line != "" {
				reason += ": " + line
			}
		}
		return r.fail(v.ReasonCode, reason)
	case v.Outcome == protocol.OutcomeNoChange:
		if !r.supports("outcome") {
			// A server without `outcome` knows no no_change: the old failure, as before.
			return r.fail(codeNoChanges, "the tool made no changes")
		}
		r.info("Nothing to change: the tool found the change already in place.")
		return protocol.StatusReport{Status: protocol.StatusSucceeded, Outcome: protocol.OutcomeNoChange}
	}
	return protocol.StatusReport{
		Status:     protocol.StatusSucceeded,
		Outcome:    protocol.OutcomeChanged,
		Branch:     r.branch,
		HeadCommit: head,
		PRURL:      r.publish(ctx, message),
	}
}

// publish pushes the branch and opens the pull request, or, when the run continues an open one, the push updates it
// (pr_updated). Neither can fail a run that has commits: what went wrong becomes a report note. A run that starts
// over closes the pull request it supersedes once its own opened.
func (r *Run) publish(ctx context.Context, message string) string {
	var open *protocol.PullRequest
	if c := r.Start.Continue; c != nil && c.PR.Open() {
		open = c.PR
	}
	if !r.push(ctx) {
		if open != nil {
			return open.URL // the pull request is there, without this round (the note says why)
		}
		return ""
	}
	if open != nil {
		text := firstNonEmpty(open.URL, "#"+strconv.Itoa(open.Number))
		r.addNote(protocol.Note{Code: notePRUpdated, Text: text})
		r.info(fmt.Sprintf("Pull request #%d updated: %s", open.Number, text))
		return open.URL
	}
	if !r.Start.Options.OpenPR {
		return ""
	}
	url, note := r.openPR(ctx, message)
	if note != nil {
		if note.Code != notePRExists {
			r.warn("Pull request not opened: " + note.Text)
		}
		r.addNote(*note)
	}
	if url != "" {
		r.info("Pull request: " + url)
		r.supersede(ctx, url)
	}
	return url
}

// push sends the run's branch to origin when push is on; a failure is a note, never the run's failure.
func (r *Run) push(ctx context.Context) bool {
	if !r.Config.PushEnabled() {
		r.addNote(protocol.Note{Code: notePushDisabled, Text: fmt.Sprintf("push is off in config.json: %s stays on this machine", r.branch)})
		return false
	}
	unlock := r.lockRepo()
	err := r.repo.Push(ctx, r.branch)
	unlock()
	if err != nil {
		r.warn("Push failed: " + err.Error())
		r.addNote(protocol.Note{Code: notePushFailed, Text: oneLine(err.Error(), maxReason)})
		return false
	}
	r.info(fmt.Sprintf("Pushed `%s` to origin.", r.branch))
	return true
}

// preserve keeps what the tool did when the run did not succeed: uncommitted work is committed on the run's branch
// (`WIP: <key>: <name>` + the trailer, without hooks; stashed if even that fails), pushed when push is on, and the
// branch and commit go out with the final status. The checkout is left clean for the next run.
func (r *Run) preserve(ctx context.Context, final *protocol.StatusReport) {
	if r.repo == nil || r.base == "" {
		return // nothing ran in the checkout
	}
	clean, err := r.repo.IsClean(ctx)
	if err != nil {
		r.Log.Warn("cannot read the checkout after the run", "error", err)
		return
	}
	if !clean {
		r.refreshChanges(ctx, &toolOutcome{})
		message := r.commitMessage("WIP: ")
		if _, err := r.repo.CommitWIP(ctx, message); err != nil {
			r.warn("Could not commit the work in progress: " + oneLine(err.Error(), 500))
			if serr := r.repo.Stash(ctx, message); serr != nil {
				r.warn("Could not stash it either; the changes stay in the checkout: " + oneLine(serr.Error(), 500))
			} else {
				r.warn(fmt.Sprintf("The work in progress is in `git stash list` of %s.", r.repo.Dir))
			}
		} else {
			r.info(fmt.Sprintf("Committed the work in progress on `%s`.", r.branch))
		}
	}
	head, err := r.repo.Head(ctx)
	if err != nil || head == r.base {
		return
	}
	final.Branch, final.HeadCommit = r.branch, head
	r.push(ctx)
}

func (r *Run) commitMessage(prefix string) string {
	message := prefix + r.Start.Subject.Key
	if title := strings.TrimSpace(r.Start.Subject.Name); title != "" {
		message += ": " + title
	}
	if r.Start.Options.CommitTrailer != "" {
		message += "\n\n" + r.Start.Options.CommitTrailer
	}
	return message
}

// openPR opens the pull request with gh; what stops it is a note (gh_missing, pr_exists with the existing URL,
// pr_failed).
func (r *Run) openPR(ctx context.Context, message string) (string, *protocol.Note) {
	gh := r.ghPath()
	if gh == "" {
		return "", &protocol.Note{Code: noteGHMissing, Text: "gh is not installed: open the pull request from the pushed branch"}
	}
	title := strings.SplitN(message, "\n", 2)[0]
	body := strings.TrimSpace(r.prose)
	if body == "" && r.answer != nil {
		body = r.answer.Headline
	}
	if body == "" {
		body = "Change executed by the ProductOS Agent."
	}
	if r.Start.Subject.URL != "" {
		body += "\n\n" + r.Start.Subject.URL
	}
	if r.Start.Options.CommitTrailer != "" {
		body += "\n\n" + r.Start.Options.CommitTrailer
	}
	out, err := r.gh(ctx, gh, "pr", "create", "--title", title, "--body", body, "--head", r.branch, "--base", r.from)
	if err != nil {
		text := oneLine(firstNonEmpty(out, err.Error()), maxReason)
		if strings.Contains(strings.ToLower(out), "already exists") {
			if url, verr := r.gh(ctx, gh, "pr", "view", r.branch, "--json", "url", "--jq", ".url"); verr == nil && strings.HasPrefix(strings.TrimSpace(url), "https://") {
				url = strings.TrimSpace(url)
				return url, &protocol.Note{Code: notePRExists, Text: url}
			}
		}
		return "", &protocol.Note{Code: notePRFailed, Text: text}
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "https://") {
			return line, nil
		}
	}
	return "", nil
}

func (r *Run) gh(ctx context.Context, gh string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gh, args...)
	cmd.Dir = r.repo.Dir
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// summary is the tool's final prose (the whole of it) and the facts under it.
func (r *Run) summary(final protocol.StatusReport) string {
	var b strings.Builder
	text := strings.TrimSpace(r.prose)
	if text == "" && r.answer != nil {
		text = r.answer.Headline
	}
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	var facts []string
	if final.Branch != "" && final.HeadCommit != "" {
		facts = append(facts, fmt.Sprintf("Branch `%s` at `%s`", final.Branch, final.HeadCommit[:min(12, len(final.HeadCommit))]))
	}
	if files := r.fileStats(); files.Count > 0 {
		facts = append(facts, fmt.Sprintf("%d file(s) changed, +%d −%d", files.Count, files.Additions, files.Deletions))
	}
	if r.tests.passed+r.tests.failed > 0 {
		facts = append(facts, fmt.Sprintf("Test runs: %d passed, %d failed", r.tests.passed, r.tests.failed))
	}
	if final.PRURL != "" {
		facts = append(facts, "Pull request: "+final.PRURL)
	}
	for _, n := range r.notes {
		facts = append(facts, n.Text)
	}
	if len(facts) > 0 {
		b.WriteString("---\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return strings.TrimSpace(b.String())
}

// report is the tool's structured answer plus what the agent knows itself.
func (r *Run) report() *protocol.Report {
	rep := &protocol.Report{AgentVersion: version.String(), Tool: &protocol.ToolRef{Name: r.Start.Tool}}
	if r.ToolVersion != "" {
		v := r.ToolVersion
		rep.Tool.Version = &v
	}
	if a := r.answer; a != nil {
		rep.Claim, rep.Headline, rep.CustomerChange = a.Claim, a.Headline, a.CustomerChange
		rep.Changes, rep.Verification, rep.LeftOut, rep.OpenQuestions, rep.Evidence = a.Changes, a.Verification, a.LeftOut, a.OpenQuestions, a.Evidence
	}
	if files := r.fileStats(); files.Count > 0 {
		rep.Files = &files
	}
	if runs := r.tests.passed + r.tests.failed; runs > 0 {
		rep.Tests = &protocol.TestStats{Runs: runs, Failed: r.tests.failed}
	}
	rep.PriorCommits = r.prior
	rep.Notes = append([]protocol.Note(nil), r.notes...)
	fitReport(rep)
	return rep
}

func (r *Run) fileStats() protocol.FileStats {
	stats := protocol.FileStats{Count: len(r.reported)}
	for _, c := range r.reported {
		stats.Additions += c.Additions
		stats.Deletions += c.Deletions
	}
	return stats
}

func (r *Run) addNote(n protocol.Note) {
	for _, have := range r.notes {
		if have == n {
			return
		}
	}
	if len(r.notes) < 30 {
		r.notes = append(r.notes, n)
	}
}

func (r *Run) supports(feature string) bool {
	return r.Supports != nil && r.Supports(feature)
}

func (r *Run) status(ctx context.Context, status string) error {
	return r.Reporter.Status(ctx, r.Start.RunID, protocol.StatusReport{Status: status})
}

// fail ends the run with a reason code and a one-line reason.
func (r *Run) fail(code, reason string) protocol.StatusReport {
	reason = oneLine(reason, maxReason)
	r.Log.Warn("run failed", "code", code, "reason", reason)
	return protocol.StatusReport{Status: protocol.StatusFailed, ReasonCode: code, Reason: reason}
}

func (r *Run) info(text string) { r.emitter.Emit(protocol.Log("info", text)) }
func (r *Run) warn(text string) { r.emitter.Emit(protocol.Log("warn", text)) }

// --- helpers ---

// clip cuts at n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// trimInput keeps tool inputs short in the drawer: long string fields (file contents) are clipped.
func trimInput(input map[string]any) any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		if s, ok := v.(string); ok && len(s) > 500 {
			cut := 500
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			out[k] = s[:cut] + fmt.Sprintf("… (%d more chars)", len(s)-cut)
			continue
		}
		out[k] = v
	}
	return out
}

type limitedWriter struct {
	w   *bytes.Buffer
	max int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.w.Len() >= l.max {
		// keep the tail: drop the first half, in place (the caller reads the same buffer afterwards)
		keep := append([]byte{}, l.w.Bytes()[l.w.Len()/2:]...)
		l.w.Reset()
		l.w.Write(keep)
	}
	l.w.Write(p)
	return len(p), nil
}
