package runner

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/productos-technology/productos-agent/internal/config"
)

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestClaudeCommand(t *testing.T) {
	d, _ := DriverFor("claude")
	cmd := d.Command(context.Background(), Invocation{Bin: "/bin/claude", Dir: "/w", Config: &config.Config{ClaudeArgs: []string{"--model", "opus"}}})
	args := cmd.Args[1:]
	if args[0] != "-p" || argAfter(args, "--output-format") != "stream-json" || !slices.Contains(args, "--verbose") || argAfter(args, "--permission-mode") != "acceptEdits" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(argAfter(args, "--allowedTools"), "StructuredOutput") || !strings.Contains(argAfter(args, "--allowedTools"), "AskUserQuestion") {
		t.Errorf("allowedTools = %q", argAfter(args, "--allowedTools"))
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(argAfter(args, "--json-schema")), &schema); err != nil || schema["additionalProperties"] != false || len(schema["required"].([]any)) != 10 {
		t.Errorf("--json-schema = %q (%v)", argAfter(args, "--json-schema"), err)
	}
	if strings.Contains(argAfter(args, "--json-schema"), "\n") {
		t.Error("the schema argument must be one line")
	}
	if slices.Contains(args, "--bare") || slices.Contains(args, "--resume") {
		t.Errorf("args = %v", args)
	}
	if args[len(args)-2] != "--model" || cmd.Dir != "/w" {
		t.Errorf("config args go last: %v", args)
	}
	for _, kv := range []string{"CI=1", "PRODUCTOS_AGENT=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1", "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "NO_COLOR=1"} {
		if !slices.Contains(cmd.Env, kv) {
			t.Errorf("env lacks %s", kv)
		}
	}

	resumed := d.Command(context.Background(), Invocation{Bin: "claude", SessionID: "s-1", Config: &config.Config{ClaudePermissionMode: "bypassPermissions"}})
	if argAfter(resumed.Args, "--resume") != "s-1" || argAfter(resumed.Args, "--permission-mode") != "bypassPermissions" || argAfter(resumed.Args, "--json-schema") == "" {
		t.Errorf("resumed args = %v", resumed.Args)
	}
}

func TestCodexCommand(t *testing.T) {
	d, _ := DriverFor("codex")
	if !d.SchemaFile() || d.AsksWithTool() {
		t.Fatal("codex reads the schema from a file and has no question tool")
	}
	cmd := d.Command(context.Background(), Invocation{Bin: "codex", SchemaFile: "/tmp/s.json", Config: &config.Config{CodexArgs: []string{"--model", "o3"}}})
	args := cmd.Args[1:]
	if args[0] != "exec" || !slices.Contains(args, "--json") || argAfter(args, "--sandbox") != "workspace-write" || argAfter(args, "--output-schema") != "/tmp/s.json" || !slices.Contains(args, "--skip-git-repo-check") {
		t.Errorf("args = %v", args)
	}
	if slices.Contains(args, "--full-auto") {
		t.Error("--full-auto was removed from codex exec")
	}
	if args[len(args)-1] != "-" || argAfter(args, "--model") != "o3" {
		t.Errorf("args = %v", args)
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") || !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Error("codex gets the same environment")
	}

	resumed := d.Command(context.Background(), Invocation{Bin: "codex", SessionID: "th-1", SchemaFile: "/tmp/s.json", Config: &config.Config{CodexSandbox: "danger-full-access"}})
	args = resumed.Args[1:]
	i := slices.Index(args, "resume")
	if i < 0 || args[i+1] != "th-1" || args[len(args)-1] != "-" {
		t.Fatalf("resumed args = %v", args)
	}
	if j := slices.Index(args, "--output-schema"); j < 0 || j > i || argAfter(args, "--sandbox") != "danger-full-access" {
		t.Errorf("the exec flags come before resume: %v", args)
	}
}
