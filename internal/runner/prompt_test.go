package runner

import (
	"strings"
	"testing"

	"github.com/Hamdan85/productos-agent/internal/protocol"
)

func TestBuildPromptFramesTheDefinition(t *testing.T) {
	start := protocol.RunStart{
		Repository:       protocol.RunRepository{DefaultBranch: "main"},
		ChangeDefinition: protocol.ChangeDefinition{Markdown: "# MOB-P7 Offline drafts\n\nDo the thing."},
		Options:          protocol.RunOptions{CommitTrailer: "ProductOS: MOB-P7", RunTests: true},
	}
	p := buildPrompt(start, "/Users/pat/mobile", "productos/mob-p7-offline-drafts", true)
	for _, want := range []string{"/Users/pat/mobile", "`productos/mob-p7-offline-drafts`", "`main`", "ProductOS: MOB-P7", "AskUserQuestion", "# MOB-P7 Offline drafts", "Do the thing."} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !strings.HasSuffix(p, "\n") {
		t.Error("prompt must end with a newline")
	}
	codex := buildPrompt(start, "/x", "b", false)
	if strings.Contains(codex, "AskUserQuestion") || !strings.Contains(codex, "single clear question") {
		t.Error("codex prompt should ask in prose")
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
