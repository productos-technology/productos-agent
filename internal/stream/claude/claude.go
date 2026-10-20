// Package claude parses Claude Code's `--output-format stream-json` lines (one JSON object per line: system init,
// assistant and user messages with content blocks, the final result) into stream items.
package claude

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/stream"
)

// AskTool is the tool Claude Code uses to ask the person something.
const AskTool = "AskUserQuestion"

// ReportTool is the tool Claude Code calls to answer --json-schema. It is the run's report, not work: the drawer never
// shows it, and its input is the fallback when the result line carries no structured_output.
const ReportTool = "StructuredOutput"

// EditTools change files; the runner diffs the checkout after each of their results.
var EditTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// ShellTools run commands; their results may be tests.
var ShellTools = map[string]bool{"Bash": true, "PowerShell": true}

// Parser keeps the pending tool calls so results can name their tool.
type Parser struct {
	pending   map[string]pendingCall
	sessionID string
	// lastText is the last assistant prose: the summary when the result itself is the structured answer's JSON.
	lastText   string
	structured json.RawMessage
	now        func() time.Time
}

type pendingCall struct {
	name    string
	input   map[string]any
	started time.Time
}

func New() *Parser { return &Parser{pending: map[string]pendingCall{}, now: time.Now} }

type line struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Message         *message        `json:"message"`
	IsError         bool            `json:"is_error"`
	Result          string          `json:"result"`
	ToolUseResult   json.RawMessage `json:"tool_use_result"`
	Error           string          `json:"error"`
	// result
	StructuredOutput  json.RawMessage `json:"structured_output"`
	PermissionDenials []denial        `json:"permission_denials"`
	APIErrorStatus    *int            `json:"api_error_status"`
	// Errors says why a session ended before it began (`--resume` of a session this machine does not have: "No
	// conversation found with session ID: …").
	Errors []string `json:"errors"`
}

type denial struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
}

// Feed parses one line. Lines that are not JSON become Log items (level info) so nothing is lost.
func (p *Parser) Feed(raw string) []stream.Item {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	at := p.now()
	var l line
	if err := json.Unmarshal([]byte(raw), &l); err != nil || l.Type == "" {
		return []stream.Item{{Kind: stream.Log, Level: "info", Text: truncate(raw, 2000), At: at}}
	}
	var items []stream.Item
	if l.SessionID != "" && l.SessionID != p.sessionID {
		p.sessionID = l.SessionID
		items = append(items, stream.Item{Kind: stream.Session, SessionID: l.SessionID, At: at})
	}
	switch l.Type {
	case "assistant":
		if l.ParentToolUseID != nil && *l.ParentToolUseID != "" {
			return items // a subagent's internals: keep the drawer short
		}
		if l.Error != "" {
			items = append(items, stream.Item{Kind: stream.Log, Level: "error", Text: l.Error + ": " + textOf(l.Message), At: at})
			return items
		}
		for _, b := range blocksOf(l.Message) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					p.lastText = b.Text
					items = append(items, stream.Item{Kind: stream.Message, Text: b.Text, At: at})
				}
			case "tool_use":
				p.pending[b.ID] = pendingCall{name: b.Name, input: b.Input, started: at}
				if b.Name == ReportTool {
					if raw, err := json.Marshal(b.Input); err == nil {
						p.structured = raw
					}
					continue
				}
				if b.Name == AskTool {
					items = append(items, questionOf(b.Input, at))
					continue
				}
				item := stream.Item{Kind: stream.ToolUse, ID: b.ID, Name: b.Name, Input: b.Input, At: at}
				if ShellTools[b.Name] {
					item.Command, _ = b.Input["command"].(string)
				}
				items = append(items, item)
			}
		}
	case "user":
		if l.ParentToolUseID != nil && *l.ParentToolUseID != "" {
			return items
		}
		for _, b := range blocksOf(l.Message) {
			if b.Type != "tool_result" {
				continue
			}
			call, known := p.pending[b.ToolUseID]
			delete(p.pending, b.ToolUseID)
			if known && call.name == AskTool {
				continue // the question's refusal in non-interactive mode: the runner handles the question itself
			}
			if known && call.name == ReportTool {
				continue
			}
			item := stream.Item{Kind: stream.ToolResult, ID: b.ToolUseID, At: at, Output: contentText(b.Content)}
			if known {
				item.Name, item.Input = call.name, call.input
				if ShellTools[call.name] {
					item.Command, _ = call.input["command"].(string)
				}
			}
			if b.IsError != nil {
				item.IsError = *b.IsError
			}
			if code, ok := exitCodeOf(l.ToolUseResult, item.Output); ok {
				item.ExitCode = &code
				if code != 0 {
					item.IsError = true
				}
			}
			if known && EditTools[call.name] && !item.IsError {
				if path, _ := call.input["file_path"].(string); path != "" {
					items = append(items, stream.Item{Kind: stream.FileChange, Path: path, Action: "modified", At: at})
				}
			}
			items = append(items, item)
		}
	case "result":
		item := stream.Item{Kind: stream.Result, Text: l.Result, Subtype: l.Subtype, IsFailure: l.IsError || (l.Subtype != "" && l.Subtype != "success"), At: at}
		if l.Subtype == "error_max_structured_output_retries" {
			// The tool did its work but never answered the schema: not a failure, the outcome comes from git alone.
			item.IsFailure = false
			item.Text = p.lastText
		}
		if l.APIErrorStatus != nil {
			item.ErrorStatus = *l.APIErrorStatus
		}
		if len(l.StructuredOutput) > 0 && string(l.StructuredOutput) != "null" {
			item.Structured = l.StructuredOutput
		} else if p.structured != nil && !item.IsFailure {
			item.Structured = p.structured
		}
		if item.Structured != nil {
			// `result` is then the answer's JSON string: the summary is the last prose message instead.
			item.Text = p.lastText
		}
		for _, d := range l.PermissionDenials {
			item.Denials = append(item.Denials, stream.Denial{Tool: d.ToolName, Input: d.ToolInput})
		}
		if item.IsFailure && item.Text == "" {
			for _, e := range l.Errors {
				if strings.TrimSpace(e) != "" {
					item.Text = strings.TrimSpace(e)
					break
				}
			}
		}
		if item.IsFailure && item.Text == "" {
			item.Text = l.Subtype
		}
		items = append(items, item)
	case "system":
		// init, hook_started, thinking_tokens, …: nothing the drawer needs.
	}
	return items
}

// Flush reports tool calls that never got a result.
func (p *Parser) Flush() []stream.Item {
	var items []stream.Item
	for id, call := range p.pending {
		if call.name == AskTool || call.name == ReportTool {
			continue
		}
		items = append(items, stream.Item{Kind: stream.ToolResult, ID: id, Name: call.name, Input: call.input, IsError: true, Output: "no result (the tool exited)", At: p.now()})
	}
	p.pending = map[string]pendingCall{}
	return items
}

func blocksOf(m *message) []block {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil && text != "" {
		return []block{{Type: "text", Text: text}}
	}
	return nil
}

func textOf(m *message) string {
	var parts []string
	for _, b := range blocksOf(m) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// contentText flattens a tool_result content (a string, or text blocks).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// exitCodeOf reads tool_use_result.exit_code / exitCode when present, else "Exit code N" in the output.
func exitCodeOf(raw json.RawMessage, output string) (int, bool) {
	if len(raw) > 0 {
		var r struct {
			ExitCode  *int `json:"exit_code"`
			ExitCode2 *int `json:"exitCode"`
		}
		if json.Unmarshal(raw, &r) == nil {
			if r.ExitCode != nil {
				return *r.ExitCode, true
			}
			if r.ExitCode2 != nil {
				return *r.ExitCode2, true
			}
		}
	}
	if m := exitCodeRE.FindStringSubmatch(output); m != nil {
		n := 0
		for _, ch := range m[1] {
			n = n*10 + int(ch-'0')
		}
		return n, true
	}
	return 0, false
}

func questionOf(input map[string]any, at time.Time) stream.Item {
	item := stream.Item{Kind: stream.Question, Name: AskTool, At: at}
	var texts []string
	questions, _ := input["questions"].([]any)
	for _, q := range questions {
		qm, _ := q.(map[string]any)
		if qm == nil {
			continue
		}
		if text, _ := qm["question"].(string); text != "" {
			texts = append(texts, text)
		}
		opts, _ := qm["options"].([]any)
		for _, o := range opts {
			switch v := o.(type) {
			case map[string]any:
				if label, _ := v["label"].(string); label != "" {
					item.Options = append(item.Options, label)
				}
			case string:
				item.Options = append(item.Options, v)
			}
		}
	}
	if len(texts) == 0 {
		if text, _ := input["question"].(string); text != "" {
			texts = append(texts, text)
		}
	}
	item.Text = strings.Join(texts, "\n\n")
	if item.Text == "" {
		item.Text = "The tool asked a question without text."
	}
	return item
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
