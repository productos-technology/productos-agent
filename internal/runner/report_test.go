package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/stream"
)

func TestReconcile(t *testing.T) {
	const base, other = "aaaaaaa", "bbbbbbb"
	cases := []struct {
		name  string
		claim *toolAnswer
		head  string
		want  verdict
	}{
		{"new commits are changed", &toolAnswer{Claim: claimChanged}, other, verdict{Status: "succeeded", Outcome: "changed"}},
		{"git wins over an already_done claim", &toolAnswer{Claim: claimAlreadyDone, Evidence: []string{"a.go:1"}}, other, verdict{Status: "succeeded", Outcome: "changed"}},
		{"git wins over a blocked claim", &toolAnswer{Claim: claimBlocked}, other, verdict{Status: "succeeded", Outcome: "changed"}},
		{"new commits without any answer", nil, other, verdict{Status: "succeeded", Outcome: "changed"}},
		{"already done with evidence", &toolAnswer{Claim: claimAlreadyDone, Evidence: []string{"src/drafts/store.ts:12"}}, base, verdict{Status: "succeeded", Outcome: "no_change"}},
		{"already done without evidence", &toolAnswer{Claim: claimAlreadyDone}, base, verdict{Status: "failed", ReasonCode: "no_changes", Reason: "the tool said the change was already in place but gave no evidence"}},
		{"blocked", &toolAnswer{Claim: claimBlocked, Headline: "The API has no endpoint for drafts"}, base, verdict{Status: "failed", ReasonCode: "blocked", Reason: "The API has no endpoint for drafts"}},
		{"blocked without words", &toolAnswer{Claim: claimBlocked}, base, verdict{Status: "failed", ReasonCode: "blocked", Reason: "the tool said it could not do it"}},
		{"claimed changed but nothing moved", &toolAnswer{Claim: claimChanged}, base, verdict{Status: "failed", ReasonCode: "no_changes", Reason: "the tool made no changes"}},
		{"no answer, nothing moved", nil, base, verdict{Status: "failed", ReasonCode: "no_changes", Reason: "the tool made no changes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcile(c.claim, gitFacts{Base: base, Head: c.head}); got != c.want {
				t.Errorf("reconcile = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestExtractReport(t *testing.T) {
	full := `{"claim":"already_done","headline":"Drafts persist","customer_change":null,"changes":[],"verification":[{"command":"npm test","passed":true}],"left_out":["two tabs"],"open_questions":[],"evidence":["src/drafts/store.ts:12"," "],"question":null,"options":[]}`
	cases := []struct {
		name      string
		items     []stream.Item
		wantNil   bool
		wantClaim string
		check     func(t *testing.T, a *toolAnswer)
	}{
		{name: "claude structured_output", items: []stream.Item{{Kind: stream.Message, Text: "Done."}, {Kind: stream.Result, Text: "Done.", Structured: json.RawMessage(full)}}, wantClaim: "already_done",
			check: func(t *testing.T, a *toolAnswer) {
				if a.Headline != "Drafts persist" || len(a.Evidence) != 1 || len(a.Verification) != 1 || !a.Verification[0].Passed || a.CustomerChange != nil || len(a.LeftOut) != 1 {
					t.Errorf("answer = %+v", a)
				}
			}},
		{name: "codex final message", items: []stream.Item{{Kind: stream.Result, Text: "prose", Structured: json.RawMessage(`{"claim":"changed","headline":"x","customer_change":"Drafts survive a reload.","changes":["a"]}`)}}, wantClaim: "changed",
			check: func(t *testing.T, a *toolAnswer) {
				if a.CustomerChange == nil || *a.CustomerChange != "Drafts survive a reload." || len(a.Changes) != 1 {
					t.Errorf("answer = %+v", a)
				}
			}},
		{name: "fenced block in the last message", items: []stream.Item{{Kind: stream.Message, Text: "Done.\n\n```productos-report\n{\"claim\":\"blocked\",\"headline\":\"No API\"}\n```\n"}, {Kind: stream.Result, Text: "Done."}}, wantClaim: "blocked"},
		{name: "an ill-typed field keeps the rest", items: []stream.Item{{Kind: stream.Result, Structured: json.RawMessage(`{"claim":"changed","headline":"x","changes":"not a list"}`)}}, wantClaim: "changed"},
		{name: "unknown claim is no claim", items: []stream.Item{{Kind: stream.Result, Structured: json.RawMessage(`{"claim":"maybe","headline":"x"}`)}}, wantClaim: ""},
		{name: "needs input with a question", items: []stream.Item{{Kind: stream.Result, Structured: json.RawMessage(`{"claim":"needs_input","headline":"","question":"sqlite or localStorage?","options":["sqlite","localStorage"]}`)}}, wantClaim: "needs_input",
			check: func(t *testing.T, a *toolAnswer) {
				if a.Question != "sqlite or localStorage?" || len(a.Options) != 2 {
					t.Errorf("answer = %+v", a)
				}
			}},
		{name: "plain prose", items: []stream.Item{{Kind: stream.Message, Text: "All done."}, {Kind: stream.Result, Text: "All done."}}, wantNil: true},
		{name: "json that is not an answer", items: []stream.Item{{Kind: stream.Result, Structured: json.RawMessage(`{"other":1}`)}}, wantNil: true},
		{name: "broken json", items: []stream.Item{{Kind: stream.Result, Structured: json.RawMessage(`{"claim":`)}}, wantNil: true},
		{name: "nothing", wantNil: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := extractReport(c.items)
			if c.wantNil {
				if a != nil {
					t.Fatalf("want nil, got %+v", a)
				}
				return
			}
			if a == nil {
				t.Fatal("want an answer, got nil")
			}
			if a.Claim != c.wantClaim {
				t.Errorf("claim = %q, want %q", a.Claim, c.wantClaim)
			}
			if c.check != nil {
				c.check(t, a)
			}
		})
	}
}

func TestToolFailureCodes(t *testing.T) {
	cases := []struct {
		subtype string
		status  int
		text    string
		want    string
	}{
		{"error_max_turns", 0, "", codeToolLimit},
		{"error_max_budget_usd", 0, "", codeToolLimit},
		{"error_during_execution", 0, "boom", codeToolError},
		{"success", 401, "API Error", codeToolAuth},
		{"success", 0, "Invalid API key · Please run /login", codeToolAuth},
		{"success", 0, "OAuth token has expired", codeToolAuth},
		{"success", 0, "Claude AI usage limit reached", codeToolLimit},
		{"", 0, "stream disconnected before completion: context window exceeded", codeToolLimit},
		{"", 0, "the turn failed", codeToolError},
	}
	for _, c := range cases {
		if got := toolFailureCode(c.subtype, c.status, c.text); got != c.want {
			t.Errorf("toolFailureCode(%q, %d, %q) = %q, want %q", c.subtype, c.status, c.text, got, c.want)
		}
	}
	if crashCode("Error: Not logged in. Run codex login") != codeToolAuth || crashCode("segmentation fault") != codeToolCrashed {
		t.Error("crashCode")
	}
}

func TestDenialNote(t *testing.T) {
	n := denialNote(stream.Denial{Tool: "Bash", Input: map[string]any{"command": "rm -rf\n/tmp/x"}})
	if n.Code != "permission_denied" || n.Text != "Bash: rm -rf /tmp/x" {
		t.Errorf("note = %+v", n)
	}
}

func TestFitReportStaysUnder32KiB(t *testing.T) {
	big := strings.Repeat("é", 3000)
	rep := &protocol.Report{Headline: big}
	for i := 0; i < 200; i++ {
		rep.Changes = append(rep.Changes, big)
		rep.Notes = append(rep.Notes, protocol.Note{Code: "x", Text: big})
	}
	fitReport(rep)
	raw, _ := json.Marshal(rep)
	if len(raw) > maxReport {
		t.Fatalf("report is %d bytes", len(raw))
	}
	if !strings.HasSuffix(rep.Headline, "…") || !utf8Valid(rep.Headline) {
		t.Errorf("headline not clipped on a rune boundary: %q", rep.Headline[len(rep.Headline)-10:])
	}
}

func TestOneLineAndFirstLine(t *testing.T) {
	if got := firstLine("\n\n  Drafts persist.  \nMore text."); got != "Drafts persist." {
		t.Errorf("firstLine = %q", got)
	}
	if got := oneLine("fatal: no\n  remote\tref", 280); got != "fatal: no remote ref" {
		t.Errorf("oneLine = %q", got)
	}
	if got := oneLine(strings.Repeat("a", 300), 280); len(got) != 280+len("…") {
		t.Errorf("oneLine length = %d", len(got))
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }
