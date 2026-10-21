package runner

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/stream"
)

// reportSchemaSource is the schema both tools answer (docs/reference/agent-protocol.md, report): strict, every field
// required, nullable where noted, no extra properties. One file, embedded, so Claude Code and Codex never drift.
//
//go:embed report_schema.json
var reportSchemaSource []byte

// reportSchema is the schema compacted to one line (Claude Code takes it as an argument).
var reportSchema = func() string {
	var b bytes.Buffer
	if err := json.Compact(&b, reportSchemaSource); err != nil {
		panic("report_schema.json: " + err.Error())
	}
	return b.String()
}()

// The tool's own words for its work.
const (
	claimChanged     = "changed"
	claimAlreadyDone = "already_done"
	claimNeedsInput  = "needs_input"
	claimBlocked     = "blocked"
)

// Reason codes the agent sends on `failed` (docs/reference/agent-protocol.md, reason_code).
const (
	codeCheckoutMissing  = "checkout_missing"
	codeCheckoutDirty    = "checkout_dirty"
	codeFetchFailed      = "fetch_failed"
	codeBranchFailed     = "branch_failed"
	codeBranchDiverged   = "branch_diverged"
	codeBranchCheckedOut = "branch_checked_out"
	codeWorktreeFailed   = "worktree_failed"
	codeToolMissing      = "tool_missing"
	codeToolAuth         = "tool_auth"
	codeToolCrashed      = "tool_crashed"
	codeToolError        = "tool_error"
	codeToolLimit        = "tool_limit"
	codeTimeout          = "timeout"
	codeTooManyQuestions = "too_many_questions"
	codeNoSession        = "no_session"
	codeNoChanges        = "no_changes"
	codeBlocked          = "blocked"
	codeCommitFailed     = "commit_failed"
	codeServerClosed     = "server_closed"
	codeAgentRestarted   = "agent_restarted"
	codeInternal         = "internal"
	// codeAttachmentFailed: a file attached to the change could not be fetched or verified; the tool never started.
	codeAttachmentFailed = "attachment_failed"
)

// AgentRestarted is the code the daemon sends for a run that was active when the process restarted.
const AgentRestarted = codeAgentRestarted

// Note codes the agent adds to report.notes.
const (
	notePushFailed       = "push_failed"
	notePushDisabled     = "push_disabled"
	notePRFailed         = "pr_failed"
	noteGHMissing        = "gh_missing"
	notePRExists         = "pr_exists"
	noteTestsFailing     = "tests_failing"
	notePermissionDenied = "permission_denied"
	notePriorCommits     = "prior_commits"
	// An adjust run: what the agent read on GitHub, and what it did with the pull request.
	noteCIRead            = "ci_read"
	noteReviewsRead       = "reviews_read"
	notePRUpdated         = "pr_updated"
	notePRSuperseded      = "pr_superseded"
	notePRSupersedeFailed = "pr_supersede_failed"
	NoteUpdateAvailable   = "update_available"
	NoteUpdated           = "updated"
	NoteUpdateFailed      = "update_failed"
	// noteAttachmentsRead: how many files attached to the change the tool was given ("2 attached files").
	noteAttachmentsRead = "attachments_read"
)

// toolAnswer is the tool's answer to the report schema.
type toolAnswer struct {
	Claim          string
	Headline       string
	CustomerChange *string
	Changes        []string
	Verification   []protocol.Verification
	LeftOut        []string
	OpenQuestions  []string
	Evidence       []string
	Question       string
	Options        []string
}

// fencedReport is the fallback for a tool that ignored the schema but followed the prompt: a ```productos-report
// block with the same JSON in its last message.
var fencedReport = regexp.MustCompile("(?s)```productos-report[ \\t]*\\r?\\n(.*?)\\r?\\n?```")

// extractReport reads the tool's structured answer from one session's items: the result's Structured answer (Claude
// Code's structured_output, Codex's final message under --output-schema), else a fenced productos-report block in the
// last assistant message, else nil (the summary keeps the prose and git alone decides the outcome).
func extractReport(items []stream.Item) *toolAnswer {
	last := ""
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Kind == stream.Result && len(it.Structured) > 0 {
			if a := parseAnswer(it.Structured); a != nil {
				return a
			}
		}
		if last == "" && it.Kind == stream.Message && strings.TrimSpace(it.Text) != "" {
			last = it.Text
		}
	}
	if m := fencedReport.FindAllStringSubmatch(last, -1); len(m) > 0 {
		return parseAnswer([]byte(m[len(m)-1][1]))
	}
	return nil
}

// parseAnswer decodes field by field, so one ill-typed field (a tool that half-complied) does not lose the rest.
func parseAnswer(raw []byte) *toolAnswer {
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(raw), &fields) != nil || fields == nil {
		return nil
	}
	a := &toolAnswer{}
	str := func(key string) string {
		var s string
		_ = json.Unmarshal(fields[key], &s)
		return strings.TrimSpace(s)
	}
	list := func(key string) []string {
		var raw []string
		_ = json.Unmarshal(fields[key], &raw)
		var out []string
		for _, s := range raw {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	switch c := str("claim"); c {
	case claimChanged, claimAlreadyDone, claimNeedsInput, claimBlocked:
		a.Claim = c
	}
	a.Headline = str("headline")
	if s := str("customer_change"); s != "" {
		a.CustomerChange = &s
	}
	a.Changes, a.LeftOut, a.OpenQuestions, a.Evidence, a.Options = list("changes"), list("left_out"), list("open_questions"), list("evidence"), list("options")
	var checks []protocol.Verification
	if json.Unmarshal(fields["verification"], &checks) == nil {
		for _, c := range checks {
			if c.Command = strings.TrimSpace(c.Command); c.Command != "" {
				a.Verification = append(a.Verification, c)
			}
		}
	}
	a.Question = str("question")
	if a.Claim == "" && a.Headline == "" && len(a.Changes) == 0 && len(a.Evidence) == 0 && a.Question == "" {
		return nil // an object, but not an answer to this schema
	}
	return a
}

// gitFacts is what git says about the run's branch once the tool is done and its work committed.
type gitFacts struct {
	Base string
	Head string
}

// verdict is how the run ends.
type verdict struct {
	Status     string
	Outcome    string
	ReasonCode string
	Reason     string
}

// reconcile decides the outcome. Git decides: new commits are `changed` whatever the tool said; an identical tree is a
// `no_change` only when the tool claimed already_done with evidence, a `blocked` failure when it said it could not, and
// a `no_changes` failure otherwise. The tool's words only explain.
func reconcile(claim *toolAnswer, facts gitFacts) verdict {
	if facts.Head != "" && facts.Head != facts.Base {
		return verdict{Status: protocol.StatusSucceeded, Outcome: protocol.OutcomeChanged}
	}
	if claim != nil {
		switch claim.Claim {
		case claimAlreadyDone:
			if len(claim.Evidence) > 0 {
				return verdict{Status: protocol.StatusSucceeded, Outcome: protocol.OutcomeNoChange}
			}
			return verdict{Status: protocol.StatusFailed, ReasonCode: codeNoChanges, Reason: "the tool said the change was already in place but gave no evidence"}
		case claimBlocked:
			return verdict{Status: protocol.StatusFailed, ReasonCode: codeBlocked, Reason: firstNonEmpty(claim.Headline, first(claim.OpenQuestions), first(claim.LeftOut), "the tool said it could not do it")}
		}
	}
	return verdict{Status: protocol.StatusFailed, ReasonCode: codeNoChanges, Reason: "the tool made no changes"}
}

// Signals in a tool's error text or stderr that name the cause.
var (
	authSignal  = regexp.MustCompile(`(?i)invalid api key|run /login|please (?:log|sign) ?in|not (?:logged|signed) in|log ?in required|oauth[^.\n]*expired|authentication[_ ]error|unauthori[sz]ed|\b401\b|codex login|credentials? (?:are |is )?(?:missing|invalid|expired)`)
	limitSignal = regexp.MustCompile(`(?i)usage limit|rate limit|\b429\b|context (?:window|length)|prompt is too long|max(?:imum)? turns|budget|quota|token limit`)
)

// toolFailureCode maps how a session ended with an error (Claude Code's subtype and API status, any tool's text).
func toolFailureCode(subtype string, status int, text string) string {
	switch subtype {
	case "error_max_turns", "error_max_budget_usd":
		return codeToolLimit
	}
	switch {
	case status == 401 || authSignal.MatchString(text):
		return codeToolAuth
	case status == 429 || limitSignal.MatchString(text):
		return codeToolLimit
	}
	return codeToolError
}

// crashCode maps a tool that exited abnormally without a result: its stderr may still say why.
func crashCode(stderr string) string {
	switch {
	case authSignal.MatchString(stderr):
		return codeToolAuth
	case limitSignal.MatchString(stderr):
		return codeToolLimit
	}
	return codeToolCrashed
}

// denialNote turns a refused tool call into a permission_denied note: the tool and what it wanted.
func denialNote(d stream.Denial) protocol.Note {
	text := firstNonEmpty(d.Tool, "a tool")
	for _, key := range []string{"command", "file_path", "path", "url", "pattern"} {
		if s, _ := d.Input[key].(string); strings.TrimSpace(s) != "" {
			text += ": " + oneLine(s, 200)
			break
		}
	}
	return protocol.Note{Code: notePermissionDenied, Text: text}
}

const (
	maxReport     = 32 << 10
	maxReportText = 1000
	maxReportList = 50
)

// fitReport keeps the report under the contract's 32 KiB: strings and lists are clipped, then halved until it fits.
func fitReport(rep *protocol.Report) {
	textMax, listMax := maxReportText, maxReportList
	for round := 0; ; round++ {
		clipReport(rep, textMax, listMax)
		raw, err := json.Marshal(rep)
		if err != nil || len(raw) <= maxReport || round > 8 {
			return
		}
		textMax, listMax = max(textMax/2, 40), max(listMax/2, 1)
	}
}

func clipReport(rep *protocol.Report, textMax, listMax int) {
	clipList := func(items []string) []string {
		if len(items) > listMax {
			items = items[:listMax]
		}
		for i := range items {
			items[i] = clip(items[i], textMax)
		}
		return items
	}
	rep.Headline = clip(rep.Headline, textMax)
	if rep.CustomerChange != nil {
		s := clip(*rep.CustomerChange, textMax)
		rep.CustomerChange = &s
	}
	rep.Changes, rep.LeftOut, rep.OpenQuestions = clipList(rep.Changes), clipList(rep.LeftOut), clipList(rep.OpenQuestions)
	rep.Evidence, rep.PriorCommits = clipList(rep.Evidence), clipList(rep.PriorCommits)
	if len(rep.Verification) > listMax {
		rep.Verification = rep.Verification[:listMax]
	}
	for i := range rep.Verification {
		rep.Verification[i].Command = clip(rep.Verification[i].Command, textMax)
	}
	if len(rep.Notes) > listMax {
		rep.Notes = rep.Notes[:listMax]
	}
	for i := range rep.Notes {
		rep.Notes[i].Text = clip(rep.Notes[i].Text, textMax)
	}
}

// firstLine is the first non-empty line, trimmed: what goes to `reason` from a tool's message.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// oneLine folds whitespace (newlines included) to single spaces and clips: `reason` is one line.
func oneLine(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}

func first(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}
