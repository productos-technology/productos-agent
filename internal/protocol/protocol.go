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
	// LatestVersion is the newest agent release the server knows (null until its release job ran); Outdated says this
	// device is below the server's minimum version and cannot start runs.
	LatestVersion *string `json:"latest_version"`
	Outdated      bool    `json:"outdated"`
	CreatedAt     string  `json:"created_at"`
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

// Outcomes of a succeeded run: git decides (head != base is changed); no_change needs the tool's already_done claim
// with evidence.
const (
	OutcomeChanged  = "changed"
	OutcomeNoChange = "no_change"
)

// StatusReport is the body of run_status / POST /runs/{id}/status. Outcome, ReasonCode and Report are additive to
// protocol 1: a server without them ignores them.
type StatusReport struct {
	Status     string  `json:"status"`
	Reason     string  `json:"reason,omitempty"`
	ReasonCode string  `json:"reason_code,omitempty"`
	Outcome    string  `json:"outcome,omitempty"`
	Branch     string  `json:"branch,omitempty"`
	HeadCommit string  `json:"head_commit,omitempty"`
	PRURL      string  `json:"pr_url,omitempty"`
	Summary    string  `json:"summary,omitempty"`
	Report     *Report `json:"report,omitempty"`
}

// Report is the run's structured result: the tool's own answer (claim … evidence) plus the facts the agent knows
// itself (notes, files, tests, prior commits, tool, agent version). Every field is optional for the server.
type Report struct {
	Claim          string         `json:"claim,omitempty"`
	Headline       string         `json:"headline,omitempty"`
	CustomerChange *string        `json:"customer_change,omitempty"`
	Changes        []string       `json:"changes,omitempty"`
	Verification   []Verification `json:"verification,omitempty"`
	LeftOut        []string       `json:"left_out,omitempty"`
	OpenQuestions  []string       `json:"open_questions,omitempty"`
	Evidence       []string       `json:"evidence,omitempty"`
	Notes          []Note         `json:"notes,omitempty"`
	Files          *FileStats     `json:"files,omitempty"`
	Tests          *TestStats     `json:"tests,omitempty"`
	PriorCommits   []string       `json:"prior_commits,omitempty"`
	Tool           *ToolRef       `json:"tool,omitempty"`
	AgentVersion   string         `json:"agent_version,omitempty"`
}

type Verification struct {
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
}

// Note is a fact the agent adds to the report: push_failed, push_disabled, pr_failed, gh_missing, pr_exists,
// tests_failing, permission_denied, prior_commits, update_available, updated, update_failed.
type Note struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

type FileStats struct {
	Count     int `json:"count"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

type TestStats struct {
	Runs   int `json:"runs"`
	Failed int `json:"failed"`
}

type ToolRef struct {
	Name    string  `json:"name"`
	Version *string `json:"version"`
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
	// seed.plant
	SeedID string `json:"seed_id,omitempty"`
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
	// hello and run.start (additive): the server's capabilities and the agent version block.
	Features []string       `json:"features,omitempty"`
	Agent    *AgentVersions `json:"agent,omitempty"`
}

// HasFeature reports whether a hello announced an additive capability ("outcome", "report", "update").
func (m ServerMessage) HasFeature(name string) bool {
	for _, f := range m.Features {
		if f == name {
			return true
		}
	}
	return false
}

// AgentVersions is the server's version block: the newest release, the protocol floor, where archives live, and the
// kill switch of automatic updates.
type AgentVersions struct {
	LatestVersion  string `json:"latest_version"`
	MinimumVersion string `json:"minimum_version"`
	DownloadBase   string `json:"download_base"`
	UpdateEnabled  bool   `json:"update_enabled"`
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
	Agent            *AgentVersions
	// Resumed: the run was handed over by the previous agent process (self-update); it was acknowledged already, so
	// it starts from the checkout step without a second `starting`.
	Resumed bool
}

// AsRunStart extracts a RunStart from a `run.start` envelope.
func (m ServerMessage) AsRunStart() RunStart {
	start := RunStart{RunID: m.RunID, Tool: m.Tool, Branch: m.Branch, Agent: m.Agent}
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

// ---- Seeding a repository (seed.plant, additive to protocol 1) ----

// Seed statuses the agent reports; the server owns the rest of the machine.
const (
	SeedPending  = "pending"
	SeedPlanting = "planting"
	SeedPlanted  = "planted"
	SeedFailed   = "failed"
)

// Seed failure codes (reason_code of a failed seed).
const (
	SeedPathExists     = "path_exists"
	SeedGitMissing     = "git_missing"
	SeedInitFailed     = "init_failed"
	SeedArchiveInvalid = "archive_invalid"
	SeedCommitFailed   = "commit_failed"
	SeedInternal       = "internal"
)

// NoteRemoteFailed is the seed note for a GitHub repository that could not be created or pushed; the seed is
// planted all the same.
const NoteRemoteFailed = "remote_failed"

// Seed is AgentSeedView. For kind "zip" the files hold only the README to add when the archive has none at its root.
type Seed struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Archive   bool   `json:"archive"`
	Workspace struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
		Key  string `json:"key"`
		URL  string `json:"url"`
	} `json:"workspace"`
	Product struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"product"`
	LocalPath          string     `json:"local_path"`
	RepositoryFullName string     `json:"repository_full_name"`
	Private            bool       `json:"private"`
	Files              []SeedFile `json:"files"`
	Commit             struct {
		Message string `json:"message"`
		Trailer string `json:"trailer"`
	} `json:"commit"`
	Status string `json:"status"`
}

type SeedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SeedStatusReport is the body of POST /agent/seeds/{id}/status.
type SeedStatusReport struct {
	Status        string `json:"status"`
	LocalPath     string `json:"local_path,omitempty"`
	HeadCommit    string `json:"head_commit,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	ReasonCode    string `json:"reason_code,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Notes         []Note `json:"notes,omitempty"`
}
