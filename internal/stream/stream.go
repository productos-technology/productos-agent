// Package stream defines the tool-neutral items the runner consumes; the claude and codex subpackages parse each
// tool's JSON stream into them.
package stream

import (
	"encoding/json"
	"time"
)

// Kind of an Item.
type Kind int

const (
	// Message is assistant prose (Markdown).
	Message Kind = iota
	// ToolUse starts a tool call (the result arrives as ToolResult with the same ID).
	ToolUse
	// ToolResult ends a tool call.
	ToolResult
	// FileChange is a tool-reported edit (Codex item, Claude Write/Edit result); the runner computes real stats.
	FileChange
	// Question is the tool asking the person something.
	Question
	// Result ends the session (success or error).
	Result
	// Log is a line worth keeping (warnings, raw unknown lines in verbose mode).
	Log
	// Session announces the session/thread id (needed to resume).
	Session
)

// Item is one parsed unit.
type Item struct {
	Kind Kind
	At   time.Time

	// Message / Question / Result / Log
	Text string
	// Question
	Options []string
	// ToolUse / ToolResult
	ID       string
	Name     string
	Input    map[string]any
	Command  string // for shell tools: the command line
	Output   string
	IsError  bool
	ExitCode *int
	// FileChange
	Path   string
	Action string
	// Result
	IsFailure bool
	// Subtype is the tool's own word for how the session ended (Claude Code: success, error_max_turns, …).
	Subtype string
	// ErrorStatus is the HTTP status of the API error that ended the session, when the tool says (Claude Code).
	ErrorStatus int
	// Structured is the tool's answer to the report schema (Claude Code's structured_output, Codex's final message).
	Structured json.RawMessage
	// Denials are the tool calls the permission mode refused.
	Denials []Denial
	// Session
	SessionID string
	// Log
	Level string
}

// Denial is one tool call the tool's permission mode refused (Claude Code's permission_denials).
type Denial struct {
	Tool  string
	Input map[string]any
}

// Parser turns one line of the tool's output into zero or more items. Implementations keep per-session state
// (pending tool calls, the session id).
type Parser interface {
	Feed(line string) []Item
	// Flush returns whatever the parser still holds when the stream ends (e.g. an unfinished tool call).
	Flush() []Item
}

// IsTestCommand is the shared heuristic that turns a shell command into a `test` event.
func IsTestCommand(cmd string) bool { return testCommand.MatchString(cmd) }
