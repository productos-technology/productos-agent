package runner

import (
	"context"
	"os/exec"

	"github.com/Hamdan85/productos-agent/internal/config"
	"github.com/Hamdan85/productos-agent/internal/stream"
	"github.com/Hamdan85/productos-agent/internal/stream/claude"
	"github.com/Hamdan85/productos-agent/internal/stream/codex"
	"github.com/Hamdan85/productos-agent/internal/tools"
)

// Driver knows how to start a tool non-interactively with a JSON stream on stdout, resume it, and parse it.
type Driver interface {
	Name() string
	// Command builds the process; the prompt goes to stdin. sessionID resumes an earlier session when not empty.
	Command(ctx context.Context, bin, dir, sessionID string, cfg *config.Config) *exec.Cmd
	NewParser() stream.Parser
	// AsksWithTool reports whether the tool has an explicit question tool (Claude Code's AskUserQuestion).
	AsksWithTool() bool
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

// --- Claude Code ---

type claudeDriver struct{}

func (claudeDriver) Name() string       { return tools.Claude }
func (claudeDriver) AsksWithTool() bool { return true }
func (claudeDriver) NewParser() stream.Parser {
	return claude.New()
}

// Tools Claude Code may use without a prompt: edits (acceptEdits), the shell for tests and git, reading and
// searching, sub-agents, and the question tool. Verified against `claude --help` 2.1.x.
const claudeAllowedTools = "Bash,Edit,Write,MultiEdit,NotebookEdit,Read,Glob,Grep,LS,WebFetch,WebSearch,Task,Agent,TodoWrite,AskUserQuestion,Skill"

func (claudeDriver) Command(ctx context.Context, bin, dir, sessionID string, cfg *config.Config) *exec.Cmd {
	mode := "acceptEdits"
	if cfg != nil && cfg.ClaudePermissionMode != "" {
		mode = cfg.ClaudePermissionMode
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", mode, "--allowedTools", claudeAllowedTools}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if cfg != nil {
		args = append(args, cfg.ClaudeArgs...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "CI=1", "PRODUCTOS_AGENT=1")
	return cmd
}

// --- Codex ---

type codexDriver struct{}

func (codexDriver) Name() string       { return tools.Codex }
func (codexDriver) AsksWithTool() bool { return false }
func (codexDriver) NewParser() stream.Parser {
	return codex.New()
}

// `codex exec --json` streams JSONL events; `--full-auto` lets it edit and run commands in the workspace sandbox
// without approvals; `-` reads the prompt from stdin. Resuming: `codex exec resume <thread_id>`.
func (codexDriver) Command(ctx context.Context, bin, dir, sessionID string, cfg *config.Config) *exec.Cmd {
	var args []string
	if sessionID != "" {
		args = []string{"exec", "resume", sessionID, "--json", "--skip-git-repo-check"}
	} else {
		args = []string{"exec", "--json", "--full-auto", "--skip-git-repo-check"}
	}
	if cfg != nil {
		args = append(args, cfg.CodexArgs...)
	}
	args = append(args, "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "CI=1", "PRODUCTOS_AGENT=1")
	return cmd
}
