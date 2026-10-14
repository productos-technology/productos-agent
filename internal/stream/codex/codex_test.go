package codex

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/productos-technology/productos-agent/internal/stream"
)

func feedFile(t *testing.T, name string) []stream.Item {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := New()
	var items []stream.Item
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		items = append(items, p.Feed(sc.Text())...)
	}
	return append(items, p.Flush()...)
}

func TestSimpleRun(t *testing.T) {
	items := feedFile(t, "simple_run.jsonl")
	if items[0].Kind != stream.Session || items[0].SessionID != "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("first = %+v", items[0])
	}
	var uses, results, changes, messages int
	var test *stream.Item
	for i := range items {
		it := items[i]
		switch it.Kind {
		case stream.ToolUse:
			uses++
		case stream.ToolResult:
			results++
			if stream.IsTestCommand(it.Command) {
				test = &items[i]
			}
		case stream.FileChange:
			changes++
		case stream.Message:
			messages++
		}
	}
	if uses != 3 || results != 3 {
		t.Errorf("uses=%d results=%d", uses, results)
	}
	if changes != 2 {
		t.Errorf("changes=%d", changes)
	}
	if messages != 2 {
		t.Errorf("messages=%d (reasoning must not count)", messages)
	}
	if test == nil || test.IsError || test.ExitCode == nil || *test.ExitCode != 0 {
		t.Errorf("test = %+v", test)
	}
	final := items[len(items)-1]
	if final.Kind != stream.Result || final.IsFailure || final.Text != "Drafts now persist in sqlite and the four drafts tests pass." {
		t.Errorf("final = %+v", final)
	}
}

func TestFileChangeKinds(t *testing.T) {
	items := feedFile(t, "simple_run.jsonl")
	var actions []string
	for _, it := range items {
		if it.Kind == stream.FileChange {
			actions = append(actions, it.Action)
		}
	}
	if len(actions) != 2 || actions[0] != "modified" || actions[1] != "created" {
		t.Errorf("actions = %v", actions)
	}
}

func TestQuestionTurnEndsWithTheQuestionAsResult(t *testing.T) {
	items := feedFile(t, "question.jsonl")
	final := items[len(items)-1]
	if final.Kind != stream.Result || final.IsFailure {
		t.Fatalf("final = %+v", final)
	}
	if final.Text != "The change definition does not say where drafts should live offline. Should I use sqlite or localStorage?" {
		t.Errorf("text = %q", final.Text)
	}
}

func TestFailedTurn(t *testing.T) {
	items := feedFile(t, "failed_turn.jsonl")
	var sawErrorLog bool
	for _, it := range items {
		if it.Kind == stream.Log && it.Level == "error" {
			sawErrorLog = true
		}
		if it.Kind == stream.ToolResult && (!it.IsError || *it.ExitCode != 101) {
			t.Errorf("cargo test should be a failed result: %+v", it)
		}
	}
	final := items[len(items)-1]
	if !sawErrorLog || final.Kind != stream.Result || !final.IsFailure {
		t.Errorf("final = %+v sawErrorLog=%v", final, sawErrorLog)
	}
}

// With --output-schema the final agent message is the report's JSON: it is the result's Structured answer, and the
// summary stays the last prose message.
func TestSchemaFinalMessageIsTheReport(t *testing.T) {
	items := feedFile(t, "structured_output.jsonl")
	final := items[len(items)-1]
	if final.Kind != stream.Result || final.IsFailure {
		t.Fatalf("final = %+v", final)
	}
	if !strings.Contains(string(final.Structured), `"claim":"already_done"`) {
		t.Errorf("structured = %s", final.Structured)
	}
	if !strings.HasPrefix(final.Text, "Drafts already persist in sqlite") {
		t.Errorf("text = %q", final.Text)
	}
	for _, it := range items {
		if it.Kind == stream.Message && strings.HasPrefix(it.Text, "{") {
			t.Errorf("the report JSON leaked as a message: %q", it.Text)
		}
	}
}

func TestReportJSON(t *testing.T) {
	for text, want := range map[string]bool{
		`{"claim":"changed","headline":"x"}`:    true,
		"```json\n{\"claim\":\"blocked\"}\n```": true,
		`{"other":1}`:                           false,
		"Should I use sqlite or localStorage?":  false,
		`{"claim": broken`:                      false,
	} {
		if _, ok := reportJSON(text); ok != want {
			t.Errorf("reportJSON(%q) = %v, want %v", text, ok, want)
		}
	}
}
