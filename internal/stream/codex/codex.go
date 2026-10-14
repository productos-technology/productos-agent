// Package codex parses `codex exec --json` lines (thread.started, turn.*, item.started/item.completed with typed
// items, error) into stream items. With --output-schema the final agent message is the report's JSON: it becomes the
// result's Structured answer, never a drawer message.
package codex

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/stream"
)

type Parser struct {
	threadID    string
	lastMessage string
	structured  json.RawMessage
	started     map[string]time.Time
	failed      bool
	failure     string
	now         func() time.Time
}

func New() *Parser { return &Parser{started: map[string]time.Time{}, now: time.Now} }

type line struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     *item  `json:"item"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Changes          []change        `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Query            string          `json:"query"`
	Message          string          `json:"message"`
}

type change struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

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
	switch l.Type {
	case "thread.started":
		if l.ThreadID != "" {
			p.threadID = l.ThreadID
			return []stream.Item{{Kind: stream.Session, SessionID: l.ThreadID, At: at}}
		}
	case "item.started":
		if l.Item != nil {
			p.started[l.Item.ID] = at
			if l.Item.Type == "command_execution" {
				return []stream.Item{{Kind: stream.ToolUse, ID: l.Item.ID, Name: "command", Command: l.Item.Command, Input: map[string]any{"command": l.Item.Command}, At: at}}
			}
			if l.Item.Type == "mcp_tool_call" {
				return []stream.Item{{Kind: stream.ToolUse, ID: l.Item.ID, Name: l.Item.Server + "." + l.Item.Tool, Input: argumentsOf(l.Item.Arguments), At: at}}
			}
		}
	case "item.completed", "item.updated":
		if l.Item != nil {
			return p.completed(l.Item, at)
		}
	case "turn.completed":
		return []stream.Item{{Kind: stream.Result, Text: p.lastMessage, IsFailure: p.failed, Structured: p.structured, At: at}}
	case "turn.failed":
		msg := "the turn failed"
		if l.Error != nil && l.Error.Message != "" {
			msg = l.Error.Message
		}
		p.failed, p.failure = true, msg
		return []stream.Item{{Kind: stream.Result, Text: msg, IsFailure: true, At: at}}
	case "error":
		msg := l.Message
		if msg == "" && l.Error != nil {
			msg = l.Error.Message
		}
		p.failed, p.failure = true, msg
		return []stream.Item{{Kind: stream.Log, Level: "error", Text: msg, At: at}}
	}
	return nil
}

func (p *Parser) completed(it *item, at time.Time) []stream.Item {
	switch it.Type {
	case "agent_message":
		if strings.TrimSpace(it.Text) == "" {
			return nil
		}
		if raw, ok := reportJSON(it.Text); ok {
			p.structured = raw
			return nil
		}
		p.lastMessage = it.Text
		return []stream.Item{{Kind: stream.Message, Text: it.Text, At: at}}
	case "reasoning", "todo_list":
		return nil
	case "command_execution":
		if it.Status == "in_progress" {
			return nil
		}
		started, seen := p.started[it.ID]
		delete(p.started, it.ID)
		items := []stream.Item{}
		if !seen {
			items = append(items, stream.Item{Kind: stream.ToolUse, ID: it.ID, Name: "command", Command: it.Command, Input: map[string]any{"command": it.Command}, At: at})
			started = at
		}
		res := stream.Item{Kind: stream.ToolResult, ID: it.ID, Name: "command", Command: it.Command, Output: it.AggregatedOutput, At: at}
		if it.ExitCode != nil {
			code := *it.ExitCode
			res.ExitCode = &code
			res.IsError = code != 0
		} else if it.Status == "failed" {
			res.IsError = true
		}
		_ = started
		return append(items, res)
	case "file_change":
		items := make([]stream.Item, 0, len(it.Changes))
		for _, c := range it.Changes {
			action := "modified"
			switch c.Kind {
			case "add", "create", "created":
				action = "created"
			case "delete", "deleted", "remove":
				action = "deleted"
			case "rename", "renamed":
				action = "renamed"
			}
			items = append(items, stream.Item{Kind: stream.FileChange, Path: c.Path, Action: action, At: at})
		}
		return items
	case "mcp_tool_call":
		_, seen := p.started[it.ID]
		delete(p.started, it.ID)
		items := []stream.Item{}
		if !seen {
			items = append(items, stream.Item{Kind: stream.ToolUse, ID: it.ID, Name: it.Server + "." + it.Tool, Input: argumentsOf(it.Arguments), At: at})
		}
		return append(items, stream.Item{Kind: stream.ToolResult, ID: it.ID, Name: it.Server + "." + it.Tool, Output: truncate(string(it.Result), 4000), IsError: it.Status == "failed", At: at})
	case "web_search":
		return []stream.Item{{Kind: stream.ToolUse, ID: it.ID, Name: "web_search", Input: map[string]any{"query": it.Query}, At: at}}
	case "error":
		p.failed, p.failure = true, it.Message
		return []stream.Item{{Kind: stream.Log, Level: "error", Text: it.Message, At: at}}
	}
	return nil
}

// Flush: a stream that ended without turn.completed is a failure unless a message explained it.
func (p *Parser) Flush() []stream.Item {
	var items []stream.Item
	for id := range p.started {
		items = append(items, stream.Item{Kind: stream.ToolResult, ID: id, Name: "command", IsError: true, Output: "no result (the tool exited)", At: p.now()})
	}
	p.started = map[string]time.Time{}
	return items
}

// reportJSON recognises the schema's answer: a JSON object (optionally fenced) with a "claim" key.
func reportJSON(text string) (json.RawMessage, bool) {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```json")
		t = strings.TrimPrefix(t, "```")
		t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
	}
	if !strings.HasPrefix(t, "{") {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(t), &m) != nil {
		return nil, false
	}
	if _, ok := m["claim"]; !ok {
		return nil, false
	}
	return json.RawMessage(t), true
}

func argumentsOf(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	return map[string]any{"arguments": string(raw)}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
