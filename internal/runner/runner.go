// Package runner executes one `run.start`: prepares the checkout (clean tree, fetch, branch), drives the tool in
// its JSON streaming mode, turns its stream into protocol events, relays questions and answers, and finishes with a
// commit, a push, optionally a pull request, and the final status.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/productos/productos-agent/internal/config"
	"github.com/productos/productos-agent/internal/gitx"
	"github.com/productos/productos-agent/internal/protocol"
	"github.com/productos/productos-agent/internal/stream"
	"github.com/productos/productos-agent/internal/tools"
)

const (
	// RunTimeout matches AgentRun::STUCK_AFTER.
	RunTimeout   = 2 * time.Hour
	maxQuestions = 10
	maxOutput    = 1000
	maxSummary   = 20000
	maxReason    = 1000
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
	// ToolPath overrides tool detection (tests).
	ToolPath string
	// Now and Timeout are overridable for tests.
	Timeout time.Duration

	mu        sync.Mutex
	cancelled bool
	reason    string
	cancelCtx context.CancelFunc
	proc      *exec.Cmd
	answers   chan Answer

	emitter   *emitter
	repo      *gitx.Repo
	branch    string
	base      string
	sessionID string
	reported  map[string]gitx.Change
	tests     testStats
	files     int
}

type testStats struct{ passed, failed int }

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
	proc := r.proc
	cancel := r.cancelCtx
	r.mu.Unlock()
	if proc != nil {
		killProcessGroup(proc)
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

func (r *Run) isCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

// Execute runs everything and returns the final report it sent (or tried to send).
func (r *Run) Execute(parent context.Context) protocol.StatusReport {
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	r.mu.Lock()
	r.cancelCtx = cancel
	r.mu.Unlock()

	r.emitter = newEmitter(r.Start.RunID, r.Reporter, r.Log)
	final := r.execute(ctx)
	r.emitter.Close(context.Background())
	if r.isCancelled() {
		final = protocol.StatusReport{Status: protocol.StatusCancelled}
	}
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer sendCancel()
	if err := r.Reporter.Status(sendCtx, r.Start.RunID, final); err != nil && !errors.Is(err, ErrRunFinished) {
		r.Log.Error("final status not delivered", "status", final.Status, "error", err)
	}
	return final
}

func (r *Run) execute(ctx context.Context) protocol.StatusReport {
	if err := r.status(ctx, protocol.StatusStarting); err != nil {
		return r.fail("could not acknowledge the run: " + err.Error())
	}
	driver, ok := DriverFor(r.Start.Tool)
	if !ok {
		return r.fail("unknown tool " + r.Start.Tool)
	}
	bin := r.ToolPath
	if bin == "" {
		path, found := tools.Path(r.Start.Tool)
		if !found {
			return r.fail(fmt.Sprintf("%s is not installed on this machine", r.Start.Tool))
		}
		bin = path
	}
	if err := r.prepare(ctx); err != nil {
		return r.fail(err.Error())
	}
	if err := r.status(ctx, protocol.StatusRunning); err != nil {
		return r.fail("could not report the run as running: " + err.Error())
	}
	r.info(fmt.Sprintf("Branch `%s` created from `origin/%s` in %s.", r.branch, r.Start.Repository.DefaultBranch, r.repo.Dir))

	prompt := buildPrompt(r.Start, r.repo.Dir, r.branch, driver.AsksWithTool())
	promptFile, err := r.writePromptFile(prompt)
	if err != nil {
		return r.fail("could not write the change definition: " + err.Error())
	}
	defer os.Remove(promptFile)

	var lastResult string
	input := prompt
	for round := 0; ; round++ {
		if r.isCancelled() {
			return protocol.StatusReport{Status: protocol.StatusCancelled}
		}
		out, err := r.runTool(ctx, driver, bin, input)
		if r.isCancelled() {
			return protocol.StatusReport{Status: protocol.StatusCancelled}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return r.fail(fmt.Sprintf("the run exceeded %s", r.Timeout))
		}
		if dead := r.emitter.Dead(); dead != nil {
			return r.fail("the server closed the run: " + dead.Error())
		}
		if err != nil {
			return r.fail(err.Error())
		}
		lastResult = out.result
		if out.failed {
			reason := strings.TrimSpace(out.result)
			if reason == "" {
				reason = r.Start.Tool + " ended with an error"
			}
			return r.fail(reason)
		}
		question := out.question
		if question == nil && !out.changed && looksLikeQuestion(out.result) {
			question = &stream.Item{Kind: stream.Question, Text: out.result}
		}
		if question == nil {
			break
		}
		if round+1 >= maxQuestions {
			return r.fail("the tool asked too many questions")
		}
		answer, err := r.ask(ctx, question)
		if err != nil {
			if r.isCancelled() {
				return protocol.StatusReport{Status: protocol.StatusCancelled}
			}
			return r.fail(err.Error())
		}
		if out.sessionID == "" {
			return r.fail("the tool asked a question but gave no session to resume")
		}
		r.sessionID = out.sessionID
		input = answerPrompt(question.Text, answer.Text)
	}
	return r.finish(ctx, lastResult)
}

// prepare: open the checkout, insist on a clean tree, fetch, branch from origin/<default>.
func (r *Run) prepare(ctx context.Context) error {
	repo, err := gitx.Open(r.Start.Repository.LocalPath)
	if err != nil {
		return err
	}
	r.repo = repo
	clean, err := repo.IsClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("the checkout at %s has uncommitted changes; commit or stash them and run again", repo.Dir)
	}
	from := r.Start.Repository.DefaultBranch
	if from == "" {
		from, err = repo.DefaultBranch(ctx)
		if err != nil {
			return err
		}
	}
	if err := repo.Fetch(ctx, from); err != nil {
		return err
	}
	name := strings.TrimSpace(r.Start.Branch)
	if name == "" || !gitx.ValidBranch(name) {
		name = gitx.BranchName(r.Start.Subject.Key, r.Start.Subject.Name)
	}
	candidate := name
	for i := 2; repo.BranchExists(ctx, candidate) || repo.RemoteBranchExists(ctx, candidate); i++ {
		candidate = fmt.Sprintf("%s-%d", name, i)
		if i > 50 {
			return fmt.Errorf("branch %s and 50 variants already exist", name)
		}
	}
	if err := repo.CreateBranch(ctx, candidate, from); err != nil {
		return err
	}
	r.branch = candidate
	r.base, err = repo.Head(ctx)
	return err
}

func (r *Run) writePromptFile(prompt string) (string, error) {
	dir := os.TempDir()
	f, err := os.CreateTemp(dir, "productos-run-*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		r.Log.Debug("chmod on temp file", "error", err)
	}
	_, err = io.WriteString(f, prompt)
	return f.Name(), err
}

type toolOutcome struct {
	result    string
	failed    bool
	question  *stream.Item
	sessionID string
	changed   bool
}

// runTool runs one tool session (initial or resumed) to its end, streaming events as it goes.
func (r *Run) runTool(ctx context.Context, driver Driver, bin, input string) (toolOutcome, error) {
	cmd := driver.Command(ctx, bin, r.repo.Dir, r.sessionID, r.Config)
	setProcessGroup(cmd)
	cmd.Stdin = strings.NewReader(input)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return toolOutcome{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, max: 64 << 10}
	r.Log.Info("starting tool", "tool", driver.Name(), "resume", r.sessionID != "")
	if err := cmd.Start(); err != nil {
		return toolOutcome{}, fmt.Errorf("could not start %s: %w", driver.Name(), err)
	}
	r.mu.Lock()
	r.proc = cmd
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
	r.proc = nil
	r.mu.Unlock()
	r.refreshChanges(ctx, &out)

	if r.isCancelled() {
		return out, nil
	}
	if waitErr != nil && !sawResult {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 600 {
			tail = "…" + tail[len(tail)-600:]
		}
		if tail == "" {
			tail = waitErr.Error()
		}
		return out, fmt.Errorf("%s exited: %s", driver.Name(), tail)
	}
	if !sawResult && !out.failed {
		if out.result == "" {
			out.result = "the tool ended without a final message"
		}
	}
	return out, nil
}

func (r *Run) handle(ctx context.Context, item stream.Item, out *toolOutcome, pending map[string]stream.Item, sawResult *bool) {
	switch item.Kind {
	case stream.Session:
		out.sessionID = item.SessionID
	case stream.Message:
		r.emitter.Emit(protocol.Message("assistant", clip(item.Text, 20000)))
	case stream.ToolUse:
		pending[item.ID] = item
	case stream.ToolResult:
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
			} else {
				r.tests.failed++
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
		q := item
		out.question = &q
	case stream.Result:
		*sawResult = true
		out.result = item.Text
		out.failed = item.IsFailure
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
	r.files = len(r.reported)
	if r.files > 0 {
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

// finish commits what the tool left, pushes, opens the PR and builds the final report.
func (r *Run) finish(ctx context.Context, result string) protocol.StatusReport {
	key := r.Start.Subject.Key
	title := strings.TrimSpace(r.Start.Subject.Name)
	message := key
	if title != "" {
		message = key + ": " + title
	}
	if r.Start.Options.CommitTrailer != "" {
		message += "\n\n" + r.Start.Options.CommitTrailer
	}
	committed, err := r.repo.CommitAll(ctx, message)
	if err != nil {
		return r.fail("commit failed: " + err.Error())
	}
	if committed {
		r.info("Committed the remaining changes.")
	}
	head, err := r.repo.Head(ctx)
	if err != nil {
		return r.fail(err.Error())
	}
	if head == r.base {
		reason := "the tool made no changes"
		if s := strings.TrimSpace(result); s != "" {
			reason += ": " + s
		}
		return r.fail(reason)
	}
	var notes []string
	pushed := false
	if r.Config.PushEnabled() {
		if err := r.repo.Push(ctx, r.branch); err != nil {
			r.warn("Push failed: " + err.Error())
			notes = append(notes, "The branch was not pushed: "+err.Error())
		} else {
			pushed = true
			r.info(fmt.Sprintf("Pushed `%s` to origin.", r.branch))
		}
	}
	prURL := ""
	if pushed && r.Start.Options.OpenPR {
		url, err := r.openPR(ctx, message, result)
		if err != nil {
			r.warn("Pull request not opened: " + err.Error())
			notes = append(notes, "No pull request was opened: "+err.Error())
		} else if url != "" {
			prURL = url
			r.info("Opened " + url)
		}
	}
	return protocol.StatusReport{
		Status:     protocol.StatusSucceeded,
		Branch:     r.branch,
		HeadCommit: head,
		PRURL:      prURL,
		Summary:    clip(r.summary(result, head, prURL, notes), maxSummary),
	}
}

func (r *Run) openPR(ctx context.Context, title, body string) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("gh is not installed")
	}
	prTitle := strings.SplitN(title, "\n", 2)[0]
	prBody := strings.TrimSpace(body)
	if prBody == "" {
		prBody = "Change executed by the ProductOS Agent."
	}
	if r.Start.Subject.URL != "" {
		prBody += "\n\n" + r.Start.Subject.URL
	}
	if r.Start.Options.CommitTrailer != "" {
		prBody += "\n\n" + r.Start.Options.CommitTrailer
	}
	cmd := exec.CommandContext(ctx, gh, "pr", "create", "--title", prTitle, "--body", prBody, "--head", r.branch, "--base", r.Start.Repository.DefaultBranch)
	cmd.Dir = r.repo.Dir
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New(strings.TrimSpace(firstNonEmpty(string(out), err.Error())))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			return line, nil
		}
	}
	return "", nil
}

func (r *Run) summary(result, head, prURL string, notes []string) string {
	var b strings.Builder
	if s := strings.TrimSpace(result); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "- Branch `%s` at `%s`\n", r.branch, head[:min(12, len(head))])
	adds, dels := 0, 0
	for _, c := range r.reported {
		adds += c.Additions
		dels += c.Deletions
	}
	fmt.Fprintf(&b, "- %d file(s) changed, +%d −%d\n", len(r.reported), adds, dels)
	if r.tests.passed+r.tests.failed > 0 {
		fmt.Fprintf(&b, "- Test runs: %d passed, %d failed\n", r.tests.passed, r.tests.failed)
	}
	if prURL != "" {
		fmt.Fprintf(&b, "- Pull request: %s\n", prURL)
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}

func (r *Run) status(ctx context.Context, status string) error {
	return r.Reporter.Status(ctx, r.Start.RunID, protocol.StatusReport{Status: status})
}

func (r *Run) fail(reason string) protocol.StatusReport {
	r.Log.Warn("run failed", "reason", reason)
	return protocol.StatusReport{Status: protocol.StatusFailed, Reason: clip(reason, maxReason)}
}

func (r *Run) info(text string) { r.emitter.Emit(protocol.Log("info", text)) }
func (r *Run) warn(text string) { r.emitter.Emit(protocol.Log("warn", text)) }

// --- helpers ---

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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
			out[k] = s[:500] + fmt.Sprintf("… (%d more chars)", len(s)-500)
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
		// keep the tail: drop the first half
		b := l.w.Bytes()
		l.w = bytes.NewBuffer(append([]byte{}, b[len(b)/2:]...))
	}
	l.w.Write(p)
	return len(p), nil
}
