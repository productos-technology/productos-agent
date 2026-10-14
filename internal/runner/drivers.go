package runner

import (
	"context"
	"os/exec"

	"github.com/productos-technology/productos-agent/internal/config"
	"github.com/productos-technology/productos-agent/internal/stream"
	"github.com/productos-technology/productos-agent/internal/stream/claude"
	"github.com/productos-technology/productos-agent/internal/stream/codex"
	"github.com/productos-technology/productos-agent/internal/tools"
)

// Driver knows how to start a tool non-interactively with a JSON stream on stdout, resume it, and parse it.
type Driver interface {
	Name() string
	// Command builds the process; the prompt goes to stdin.
	Command(ctx context.Context, in Invocation) *exec.Cmd
	NewParser() stream.Parser
	// AsksWithTool reports whether the tool has an explicit question tool (Claude Code's AskUserQuestion).
	AsksWithTool() bool
	// SchemaFile reports whether the tool reads the report schema from a file (Codex's --output-schema); the runner
	// then writes it and passes its path in Invocation.SchemaFile.
	SchemaFile() bool
}

// Invocation is one tool session: which binary, where, which session to resume (empty for a new one), and the run's
// settings.
type Invocation struct {
	Bin        string
	Dir        string
	SessionID  string
	Config     *config.Config
	SchemaFile string
}

// DriverFor returns the driver of a protocol tool name.
func DriverFor(tool string) (Driver, bool) {
	switch tool {
	case tools.Claude:
		return claudeDriver{}, true
	case tools.Codex:
		return codexDriver{}, true
	}
	return nil, false
}

// toolEnv is what every tool session runs with: unattended (CI), no auto-memory writes (refused in this mode, they
// only add "denied" noise), no self-update in the middle of a run, no credential prompt from git or gh (a prompt
// would hang the run), no colour codes in the stream.
var toolEnv = []string{
	"CI=1",
	"PRODUCTOS_AGENT=1",
	"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	"DISABLE_AUTOUPDATER=1",
	"GIT_TERMINAL_PROMPT=0",
	"GH_PROMPT_DISABLED=1",
	"NO_COLOR=1",
}

func command(ctx context.Context, in Invocation, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, in.Bin, args...)
	cmd.Dir = in.Dir
	cmd.Env = append(cmd.Environ(), toolEnv...)
	return cmd
}

// --- Claude Code ---

type claudeDriver struct{}

func (claudeDriver) Name() string       { return tools.Claude }
func (claudeDriver) AsksWithTool() bool { return true }
func (claudeDriver) SchemaFile() bool   { return false }
func (claudeDriver) NewParser() stream.Parser {
	return claude.New()
}

// Tools Claude Code may use without a prompt: edits (acceptEdits), the shell for tests and git, reading and
// searching, sub-agents, the question tool, and StructuredOutput (how it answers --json-schema). Verified against
// `claude --help` 2.1.x.
const claudeAllowedTools = "Bash,Edit,Write,MultiEdit,NotebookEdit,Read,Glob,Grep,LS,WebFetch,WebSearch,Task,Agent,TodoWrite,AskUserQuestion,Skill," + claude.ReportTool

// `claude -p` streams JSON lines; --json-schema makes the model answer the report through StructuredOutput, read
// back from the result's structured_output. No --bare: it would skip CLAUDE.md and the OAuth sign-in.
func (claudeDriver) Command(ctx context.Context, in Invocation) *exec.Cmd {
	mode := "acceptEdits"
	if in.Config != nil && in.Config.ClaudePermissionMode != "" {
		mode = in.Config.ClaudePermissionMode
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", mode, "--allowedTools", claudeAllowedTools, "--json-schema", reportSchema}
	if in.SessionID != "" {
		args = append(args, "--resume", in.SessionID)
	}
	if in.Config != nil {
		args = append(args, in.Config.ClaudeArgs...)
	}
	return command(ctx, in, args)
}

// --- Codex ---

type codexDriver struct{}

func (codexDriver) Name() string       { return tools.Codex }
func (codexDriver) AsksWithTool() bool { return false }
func (codexDriver) SchemaFile() bool   { return true }
func (codexDriver) NewParser() stream.Parser {
	return codex.New()
}

// `codex exec --json` streams JSONL events; `--sandbox workspace-write` lets it edit and run commands in the checkout
// without approvals (`--full-auto` is gone); `--output-schema` makes its final message the report's JSON; `-` reads
// the prompt from stdin. The flags belong to `exec` and come before `resume <thread_id>`, so a resumed session keeps
// the same sandbox and schema.
func (codexDriver) Command(ctx context.Context, in Invocation) *exec.Cmd {
	sandbox := "workspace-write"
	if in.Config != nil && in.Config.CodexSandbox != "" {
		sandbox = in.Config.CodexSandbox
	}
	args := []string{"exec", "--json", "--sandbox", sandbox, "--skip-git-repo-check"}
	if in.SchemaFile != "" {
		args = append(args, "--output-schema", in.SchemaFile)
	}
	if in.Config != nil {
		args = append(args, in.Config.CodexArgs...)
	}
	if in.SessionID != "" {
		args = append(args, "resume", in.SessionID)
	}
	args = append(args, "-")
	return command(ctx, in, args)
}
