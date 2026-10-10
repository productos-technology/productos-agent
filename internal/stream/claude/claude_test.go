package claude

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"

	"github.com/Hamdan85/productos-agent/internal/stream"
)

func feedFile(t *testing.T, name string) []stream.Item {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := New()
	var items []stream.Item
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		items = append(items, p.Feed(sc.Text())...)
	}
	return append(items, p.Flush()...)
}

func kinds(items []stream.Item) []stream.Kind {
	out := make([]stream.Kind, len(items))
	for i, it := range items {
		out[i] = it.Kind
	}
	return out
}

func TestSimpleRunRecordedFromClaudeCode(t *testing.T) {
	items := feedFile(t, "simple_run.jsonl")
	want := []stream.Kind{stream.Session, stream.ToolUse, stream.FileChange, stream.ToolResult, stream.ToolUse, stream.ToolResult, stream.Message, stream.Result}
	got := kinds(items)
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d kind = %v, want %v (%+v)", i, got[i], want[i], items[i])
		}
	}
	if items[0].SessionID != "538221b4-cda1-4ac5-9eb5-507cd756de39" {
		t.Errorf("session id = %q", items[0].SessionID)
	}
	if items[1].Name != "Write" || items[2].Path != "/Users/pat/mobile/notes.txt" {
		t.Errorf("write not recognised: %+v %+v", items[1], items[2])
	}
	if items[4].Name != "Bash" || items[4].Command != "cat notes.txt" {
		t.Errorf("bash tool_use = %+v", items[4])
	}
	if items[5].Output != "ok" || items[5].IsError || items[5].Name != "Bash" {
		t.Errorf("bash result = %+v", items[5])
	}
	final := items[len(items)-1]
	if final.IsFailure || final.Text == "" {
		t.Errorf("result = %+v", final)
	}
}

func TestQuestionIsRecognised(t *testing.T) {
	items := feedFile(t, "question_and_tests.jsonl")
	var q *stream.Item
	for i := range items {
		if items[i].Kind == stream.Question {
			q = &items[i]
		}
	}
	if q == nil {
		t.Fatalf("no question in %v", kinds(items))
	}
	if q.Text != "Which storage should keep the drafts offline?" {
		t.Errorf("question text = %q", q.Text)
	}
	if len(q.Options) != 2 || q.Options[0] != "sqlite" || q.Options[1] != "localStorage" {
		t.Errorf("options = %v", q.Options)
	}
	// The tool_result of the question (an error in non-interactive mode) is not a tool result for the drawer.
	for _, it := range items {
		if it.Kind == stream.ToolResult && it.Name == AskTool {
			t.Errorf("the question's refusal leaked as a tool result: %+v", it)
		}
	}
}

func TestTestsEditsAndSubagentsOnResume(t *testing.T) {
	items := feedFile(t, "resumed_with_tests.jsonl")
	var results []stream.Item
	var changes []string
	for _, it := range items {
		switch it.Kind {
		case stream.ToolResult:
			results = append(results, it)
		case stream.FileChange:
			changes = append(changes, it.Path)
		case stream.Message:
			if it.Text == "Subagent internal reasoning that the drawer should not show." {
				t.Errorf("subagent text leaked")
			}
		}
	}
	if len(changes) != 2 || changes[0] != "/Users/pat/mobile/src/drafts/store.ts" || changes[1] != "/Users/pat/mobile/src/drafts/reload.ts" {
		t.Errorf("file changes = %v", changes)
	}
	var tests []stream.Item
	for _, r := range results {
		if r.Command != "" && stream.IsTestCommand(r.Command) {
			tests = append(tests, r)
		}
	}
	if len(tests) != 2 {
		t.Fatalf("test results = %d, want 2: %+v", len(tests), results)
	}
	if !tests[0].IsError || tests[0].ExitCode == nil || *tests[0].ExitCode != 1 {
		t.Errorf("first test run should fail with exit code 1: %+v", tests[0])
	}
	if tests[1].IsError {
		t.Errorf("second test run should pass: %+v", tests[1])
	}
	if items[len(items)-1].Kind != stream.Result || items[len(items)-1].IsFailure {
		t.Errorf("final = %+v", items[len(items)-1])
	}
}

func TestErrorResultAndGarbageLines(t *testing.T) {
	items := feedFile(t, "error_max_turns.jsonl")
	var sawLog, sawFailure bool
	for _, it := range items {
		if it.Kind == stream.Log && it.Text == "not json at all" {
			sawLog = true
		}
		if it.Kind == stream.Result && it.IsFailure && it.Text == "error_max_turns" {
			sawFailure = true
		}
	}
	if !sawLog || !sawFailure {
		t.Errorf("log=%v failure=%v items=%+v", sawLog, sawFailure, items)
	}
}

func TestFlushReportsUnfinishedCalls(t *testing.T) {
	p := New()
	p.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sleep 99"}}]},"session_id":"s"}`)
	items := p.Flush()
	if len(items) != 1 || items[0].Kind != stream.ToolResult || !items[0].IsError || items[0].ID != "t1" {
		t.Errorf("flush = %+v", items)
	}
}
