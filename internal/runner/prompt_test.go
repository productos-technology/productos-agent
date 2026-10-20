package runner

import (
	"strings"
	"testing"

	"github.com/productos-technology/productos-agent/internal/protocol"
)

func TestBuildPromptFramesTheDefinition(t *testing.T) {
	start := protocol.RunStart{
		Repository:       protocol.RunRepository{DefaultBranch: "main"},
		ChangeDefinition: protocol.ChangeDefinition{Markdown: "# MOB-P7 Offline drafts\n\nDo the thing."},
		Options:          protocol.RunOptions{CommitTrailer: "ProductOS: MOB-P7", RunTests: true},
	}
	p := buildPrompt(start, promptFacts{Dir: "/Users/pat/mobile", Branch: "productos/mob-p7-offline-drafts", AsksWithTool: true, Prior: []string{"a1b2c3d Drafts: IndexedDB store"}})
	for _, want := range []string{"/Users/pat/mobile", "`productos/mob-p7-offline-drafts`", "`main`", "ProductOS: MOB-P7", "AskUserQuestion", "# MOB-P7 Offline drafts", "Do the thing.",
		"already satisfied", "`already_done`", "file:line", "a1b2c3d Drafts: IndexedDB store", "cosmetic", "`left_out`", "write no file outside it", "language of the change definition", "structured output", "`blocked`"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !strings.HasSuffix(p, "\n") {
		t.Error("prompt must end with a newline")
	}
	codex := buildPrompt(start, promptFacts{Dir: "/x", Branch: "b"})
	if strings.Contains(codex, "AskUserQuestion") || !strings.Contains(codex, "claim `needs_input`") {
		t.Error("codex prompt should ask through the report's question")
	}
	if strings.Contains(codex, "already carry") {
		t.Error("no prior commits, no list")
	}
	if strings.Contains(p, "worktree") || strings.Contains(p, "already holds") {
		t.Error("a fresh run in the checkout says nothing of worktrees or earlier work")
	}
}

func TestAdjustPromptContinuesTheBranchInAWorktree(t *testing.T) {
	start := protocol.RunStart{
		Repository:       protocol.RunRepository{DefaultBranch: "main"},
		ChangeDefinition: protocol.ChangeDefinition{Markdown: "# MOB-C4 Keep offline drafts\n\nDo the thing."},
		Options:          protocol.RunOptions{CommitTrailer: "ProductOS: MOB-C4", RunTests: true, Worktree: true},
	}
	c := &protocol.Continue{Branch: "productos/mob-c4-keep-offline-drafts", PR: &protocol.PullRequest{Number: 25, URL: "https://github.com/acme/mobile/pull/25", State: "open"}}
	p := buildPrompt(start, promptFacts{Dir: "/state/worktrees/r/x", Branch: c.Branch, AsksWithTool: true, Worktree: true, Adjust: c, Brief: "## Adjust the previous run\n\nCI fails on 4f2a1c3.\n"})
	for _, want := range []string{"already holds the work of an earlier run", "pull request #25", "do not start over", "Read the brief first", "git worktree made for this change", "dependencies may need installing", "updates the pull request", "# MOB-C4 Keep offline drafts", "## Adjust the previous run", "CI fails on 4f2a1c3."} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "opens the pull request") || strings.Contains(p, "First check whether the change definition is already satisfied") {
		t.Error("an adjust run updates the open pull request and starts from the brief")
	}
	if strings.Index(p, "Do the thing.") > strings.Index(p, "## Adjust the previous run") {
		t.Error("the brief comes after the change definition")
	}
}

func TestLooksLikeQuestion(t *testing.T) {
	if !looksLikeQuestion("I read the store.\n\nShould I use sqlite or localStorage?\n") {
		t.Error("expected a question")
	}
	if looksLikeQuestion("Done. All tests pass.") || looksLikeQuestion("") {
		t.Error("not a question")
	}
}

func TestTrimInput(t *testing.T) {
	long := strings.Repeat("x", 2000)
	out := trimInput(map[string]any{"file_path": "a.ts", "content": long, "n": 3}).(map[string]any)
	if out["file_path"] != "a.ts" || out["n"] != 3 {
		t.Errorf("short fields must pass through: %v", out)
	}
	if s := out["content"].(string); len(s) > 560 || !strings.Contains(s, "1500 more chars") {
		t.Errorf("content not clipped: %d", len(s))
	}
}
