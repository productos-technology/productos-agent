// Package protocol is the wire contract of docs/reference/agent-protocol.md (protocol_version 1): the REST views,
// the channel messages in both directions, and the run event kinds.
package protocol

import "encoding/json"

const Version = 1

// Tools is what the device found on the machine: {"claude": {available, version}, "codex": {...}}.
type Tools map[string]ToolInfo

type ToolInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	// Path is local only (where the binary was found); never sent.
	Path string `json:"-"`
}

// Available reports whether the named tool was found.
func (t Tools) Available(name string) bool { return t[name].Available }

// Device is AgentDeviceView.
type Device struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Platform     string           `json:"platform"`
	Arch         string           `json:"arch"`
	AgentVersion string           `json:"agent_version"`
	Tools        Tools            `json:"tools"`
	Online       bool             `json:"online"`
	LastSeenAt   *string          `json:"last_seen_at"`
	RevokedAt    *string          `json:"revoked_at"`
	Repositories []RepositoryLink `json:"repositories"`
	CreatedAt    string           `json:"created_at"`
}

// RepositoryLink is AgentDeviceRepositoryView.
type RepositoryLink struct {
	ID            string    `json:"id"`
	RepositoryID  string    `json:"repository_id"`
	FullName      string    `json:"full_name"`
	DefaultBranch string    `json:"default_branch"`
	LocalPath     string    `json:"local_path"`
	RemoteURL     string    `json:"remote_url"`
	Workspace     Workspace `json:"workspace"`
	LinkedAt      string    `json:"linked_at"`
}

type Workspace struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Run is AgentRunView.
type Run struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Pain        *struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"pain"`
	Device struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
		Online   bool   `json:"online"`
	} `json:"device"`
	Repository struct {
		ID            string `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Tool          string  `json:"tool"`
	Status        string  `json:"status"`
	Branch        *string `json:"branch"`
	HeadCommit    *string `json:"head_commit"`
	PRURL         *string `json:"pr_url"`
	Summary       *string `json:"summary"`
	FailureReason *string `json:"failure_reason"`
	LastSeq       int     `json:"last_seq"`
	CreatedAt     string  `json:"created_at"`
}

// RunEvent is AgentRunEventView.
type RunEvent struct {
	Seq       int             `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// RunDetail is AgentRunDetailView.
type RunDetail struct {
	Run    Run        `json:"run"`
	Events []RunEvent `json:"events"`
}

// Statuses the device reports.
const (
	StatusQueued       = "queued"
	StatusStarting     = "starting"
	StatusRunning      = "running"
	StatusWaitingInput = "waiting_input"
	StatusSucceeded    = "succeeded"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
)

// StatusReport is the body of run_status / POST /runs/{id}/status.
type StatusReport struct {
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Branch     string `json:"branch,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
	PRURL      string `json:"pr_url,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// Event kinds the device may send.
const (
	KindMessage    = "message"
	KindToolUse    = "tool_use"
	KindFileChange = "file_change"
	KindTest       = "test"
	KindQuestion   = "question"
	KindLog        = "log"
)

// Event is one entry of a run_event batch. Payload must marshal to a JSON object.
type Event struct {
	Kind    string `json:"kind"`
	Payload any    `json:"payload"`
}

type MessagePayload struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type ToolUsePayload struct {
	Name       string `json:"name"`
	Input      any    `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type FileChangePayload struct {
	Path      string `json:"path"`
	Action    string `json:"action"` // created | modified | deleted | renamed
	Additions *int   `json:"additions,omitempty"`
	Deletions *int   `json:"deletions,omitempty"`
	From      string `json:"from,omitempty"`
}

type TestPayload struct {
	Command    string `json:"command"`
	Passed     bool   `json:"passed"`
	Summary    string `json:"summary,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type QuestionPayload struct {
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

type LogPayload struct {
	Level string `json:"level"` // info | warn | error
	Text  string `json:"text"`
}

// Constructors keep payload shapes in one place.
func Message(role, text string) Event { return Event{KindMessage, MessagePayload{role, text}} }
func Log(level, text string) Event    { return Event{KindLog, LogPayload{level, text}} }

// ---- Server -> device (the AgentChannel stream) ----

// ServerMessage is the envelope of everything the channel transmits; Type decides which fields are set.
type ServerMessage struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
	DeviceID        string `json:"device_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	// ack
	Status  string `json:"status,omitempty"`
	LastSeq *int   `json:"last_seq,omitempty"`
	// error
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
	// run.cancel
	Reason string `json:"reason,omitempty"`
	// run.answer
	Seq       int    `json:"seq,omitempty"`
	AnswerSeq int    `json:"answer_seq,omitempty"`
	Text      string `json:"text,omitempty"`
	// run.start
	Tool             string            `json:"tool,omitempty"`
	Subject          *Subject          `json:"subject,omitempty"`
	Workspace        *Workspace        `json:"workspace,omitempty"`
	Repository       *RunRepository    `json:"repository,omitempty"`
	Branch           string            `json:"branch,omitempty"`
	ChangeDefinition *ChangeDefinition `json:"change_definition,omitempty"`
	Options          *RunOptions       `json:"options,omitempty"`
	Raw              json.RawMessage   `json:"-"`
}

type Subject struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type RunRepository struct {
	ID            string `json:"id"`
	FullName      string `json:"full_name"`
	LocalPath     string `json:"local_path"`
	RemoteURL     string `json:"remote_url"`
	DefaultBranch string `json:"default_branch"`
}

type ChangeDefinition struct {
	Markdown string          `json:"markdown"`
	JSON     json.RawMessage `json:"json"`
}

type RunOptions struct {
	CommitTrailer string `json:"commit_trailer"`
	OpenPR        bool   `json:"open_pr"`
	RunTests      bool   `json:"run_tests"`
}

// RunStart is the `run.start` message as the runner consumes it.
type RunStart struct {
	RunID            string
	Tool             string
	Subject          Subject
	Workspace        Workspace
	Repository       RunRepository
	Branch           string
	ChangeDefinition ChangeDefinition
	Options          RunOptions
}

// AsRunStart extracts a RunStart from a `run.start` envelope.
func (m ServerMessage) AsRunStart() RunStart {
	start := RunStart{RunID: m.RunID, Tool: m.Tool, Branch: m.Branch}
	if m.Subject != nil {
		start.Subject = *m.Subject
	}
	if m.Workspace != nil {
		start.Workspace = *m.Workspace
	}
	if m.Repository != nil {
		start.Repository = *m.Repository
	}
	if m.ChangeDefinition != nil {
		start.ChangeDefinition = *m.ChangeDefinition
	}
	if m.Options != nil {
		start.Options = *m.Options
	}
	return start
}
