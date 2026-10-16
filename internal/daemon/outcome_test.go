package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/productos-technology/productos-agent/internal/paths"
	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/update"
)

// The release key of these tests: fakeRelease signs with it, daemons verify with it (like the compiled-in keys).
var testReleasePublic, testReleasePrivate, _ = ed25519.GenerateKey(rand.Reader)

var allFeatures = []string{"outcome", "report", "update"}

// alreadyDoneFixture is a session that reads the code, changes nothing and answers the schema with already_done.
func alreadyDoneFixture(t *testing.T, evidence string) string {
	t.Helper()
	answer := map[string]any{"claim": "already_done", "headline": "Drafts already persist across reloads", "customer_change": nil, "changes": []string{},
		"verification": []any{}, "left_out": []string{}, "open_questions": []string{}, "evidence": []string{}, "question": nil, "options": []string{}}
	if evidence != "" {
		answer["evidence"] = []string{evidence}
	}
	raw, _ := json.Marshal(answer)
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s-done"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"src/drafts/store.ts"}}]},"session_id":"s-done"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"export const store = new Map();"}]},"session_id":"s-done"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Drafts already persist: src/drafts/store.ts keeps them.\n\nNothing to change."}]},"session_id":"s-done"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"StructuredOutput","input":` + string(raw) + `}]},"session_id":"s-done"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t2","type":"tool_result","content":"Structured output provided successfully"}]},"session_id":"s-done"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":` + strconvQuote(string(raw)) + `,"structured_output":` + string(raw) + `,"permission_denials":[],"session_id":"s-done"}`,
	}
	path := filepath.Join(t.TempDir(), "already_done.jsonl")
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	return path
}

func strconvQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func lastStatus(t *testing.T, run *fakeRun) protocol.StatusReport {
	t.Helper()
	if len(run.statuses) == 0 {
		t.Fatal("no status")
	}
	return run.statuses[len(run.statuses)-1]
}

func hasNote(rep *protocol.Report, code string) (protocol.Note, bool) {
	if rep == nil {
		return protocol.Note{}, false
	}
	for _, n := range rep.Notes {
		if n.Code == code {
			return n, true
		}
	}
	return protocol.Note{}, false
}

func TestNoChangeRunSucceedsWithEvidence(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	before := git(t, origin, "rev-parse", "main")
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", alreadyDoneFixture(t, "src/drafts/store.ts:1"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "")

	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-nc", work, "productos/mob-p7-no-change")
	run := runDaemon(t, f, work)

	if got := strings.Join(statuses(run.statuses), ","); got != "starting,running,succeeded" {
		t.Fatalf("statuses = %s (final %+v)", got, lastStatus(t, run))
	}
	final := lastStatus(t, run)
	if final.Outcome != "no_change" || final.Branch != "" || final.HeadCommit != "" || final.PRURL != "" {
		t.Errorf("final = %+v", final)
	}
	if final.Report == nil || final.Report.Claim != "already_done" || len(final.Report.Evidence) != 1 || final.Report.Headline == "" {
		t.Fatalf("report = %+v", final.Report)
	}
	if final.Report.Tool == nil || final.Report.Tool.Name != "claude" || final.Report.Tool.Version == nil || *final.Report.Tool.Version != "2.1.292" || final.Report.AgentVersion == "" {
		t.Errorf("report facts = %+v", final.Report)
	}
	if !strings.HasPrefix(final.Summary, "Drafts already persist: src/drafts/store.ts keeps them.") || strings.Contains(final.Summary, `"claim"`) {
		t.Errorf("summary = %q", final.Summary)
	}
	if out := git(t, work, "status", "--porcelain"); out != "" {
		t.Errorf("checkout not clean: %q", out)
	}
	if after := git(t, origin, "rev-parse", "main"); after != before {
		t.Error("nothing may be pushed")
	}
	for _, e := range run.events {
		if e.Kind == "tool_use" && payload(t, e)["name"] == "StructuredOutput" {
			t.Error("the report tool leaked into the drawer")
		}
	}
}

func TestLegacyServerGetsTheOldFailure(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", alreadyDoneFixture(t, "src/drafts/store.ts:1"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "")

	f := newFakeServer(t) // no features: a server from before outcomes
	f.start = startMessage("run-legacy", work, "productos/mob-p7-legacy")
	run := runDaemon(t, f, work)

	final := lastStatus(t, run)
	if final.Status != "failed" || final.Reason != "the tool made no changes" || final.ReasonCode != "no_changes" || final.Outcome != "" {
		t.Errorf("final = %+v", final)
	}
}

func TestNoChangeWithoutEvidenceFails(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", alreadyDoneFixture(t, ""))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "")
	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-noev", work, "productos/mob-p7-noev")
	final := lastStatus(t, runDaemon(t, f, work))
	if final.Status != "failed" || final.ReasonCode != "no_changes" || !strings.Contains(final.Reason, "no evidence") {
		t.Errorf("final = %+v", final)
	}
}

func TestFailureAfterEditsLeavesACleanCheckoutAndAWIPCommit(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/error_max_turns.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n;src/drafts/half.ts=// half done\\n")

	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-wip", work, "productos/mob-p7-wip")
	run := runDaemon(t, f, work)

	final := lastStatus(t, run)
	if final.Status != "failed" || final.ReasonCode != "tool_limit" || final.Reason != "error_max_turns" {
		t.Fatalf("final = %+v", final)
	}
	if final.Branch != "productos/mob-p7-wip" || len(final.HeadCommit) < 7 {
		t.Fatalf("the preserved work must be reported: %+v", final)
	}
	if out := git(t, work, "status", "--porcelain"); out != "" {
		t.Errorf("checkout not clean: %q", out)
	}
	if head := git(t, origin, "rev-parse", "refs/heads/"+final.Branch); head != final.HeadCommit {
		t.Errorf("origin %s != reported %s", head, final.HeadCommit)
	}
	msg := git(t, work, "log", "-1", "--format=%B", final.HeadCommit)
	if !strings.HasPrefix(msg, "WIP: MOB-P7: Offline drafts are lost") || !strings.Contains(msg, "ProductOS: MOB-P7") {
		t.Errorf("commit message = %q", msg)
	}
	if final.Report == nil || final.Report.Files == nil || final.Report.Files.Count != 2 {
		t.Errorf("report = %+v", final.Report)
	}
}

func TestPushFailureBecomesANote(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	git(t, work, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "gone.git"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n")

	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-push", work, "productos/mob-p7-push")
	run := runDaemon(t, f, work)

	final := lastStatus(t, run)
	if final.Status != "succeeded" || final.Outcome != "changed" || len(final.HeadCommit) < 7 || final.PRURL != "" {
		t.Fatalf("final = %+v", final)
	}
	note, ok := hasNote(final.Report, "push_failed")
	if !ok || note.Text == "" || strings.Contains(note.Text, "\n") {
		t.Errorf("notes = %+v", final.Report)
	}
	if !strings.Contains(final.Summary, note.Text) {
		t.Errorf("the summary lists the note: %q", final.Summary)
	}
}

func TestHandoffIsResumedOnStart(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n")

	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-h", work, "productos/mob-p7-handoff")
	// The previous process acknowledged it: the server has it as starting and will not send run.start again.
	f.runs["run-h"] = &fakeRun{status: "starting", final: make(chan protocol.StatusReport, 1)}
	if err := writeHandoff(Handoff{From: "0.0.0", To: "0.0.0-dev", PID: 1, WrittenAt: time.Now(), Runs: []protocol.ServerMessage{f.start}, Features: allFeatures}); err != nil {
		t.Fatal(err)
	}
	run := runDaemon(t, f, work)

	if got := strings.Join(statuses(run.statuses), ","); got != "running,succeeded" {
		t.Fatalf("statuses = %s: a handed-over run skips starting", got)
	}
	final := lastStatus(t, run)
	if _, ok := hasNote(final.Report, "updated"); !ok || final.Outcome != "changed" {
		t.Errorf("final = %+v", final)
	}
	dir, _ := paths.StateDir()
	if _, err := os.Stat(filepath.Join(dir, "handoff.json")); !os.IsNotExist(err) {
		t.Error("handoff.json must be deleted once read")
	}
}

// An update due before a run: the agent stages and swaps the binary and re-executes with a handoff; here the
// re-exec fails, so the run continues on this process with an update_failed note.
func TestUpdateBeforeTheRunNeverFailsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release binary is a shell script")
	}
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n")
	t.Setenv("PRODUCTOS_AGENT_NO_UPDATE", "")

	release := fakeRelease(t, "0.2.0", "#!/bin/sh\necho \"productos-agent 0.2.0 (abc, today)\"\n")
	exe := filepath.Join(t.TempDir(), "productos-agent")
	_ = os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755)
	agent := &protocol.AgentVersions{LatestVersion: "0.2.0", MinimumVersion: "0.1.0", DownloadBase: release.URL + "/download", UpdateEnabled: true}

	f := newFakeServer(t)
	f.features = allFeatures
	f.agent = agent
	f.start = startMessage("run-up", work, "productos/mob-p7-update")
	f.start.Agent = agent
	var handedOver []string
	run := runDaemonWith(t, f, work, func(d *Daemon) {
		d.Version, d.Exe, d.HTTP, d.ReleaseKeys = "0.1.0", exe, release.Client(), []ed25519.PublicKey{testReleasePublic}
		d.Reexec = func(path string) error {
			h, err := takeHandoff()
			if err != nil || h == nil {
				return fmt.Errorf("no handoff: %v", err)
			}
			for _, m := range h.Runs {
				handedOver = append(handedOver, m.RunID)
			}
			return errors.New("exec refused in tests")
		}
	})

	if strings.Join(handedOver, ",") != "run-up" {
		t.Errorf("handoff runs = %v", handedOver)
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "0.2.0") {
		t.Errorf("the binary was not replaced: %q", got)
	}
	final := lastStatus(t, run)
	if final.Status != "succeeded" || final.Outcome != "changed" {
		t.Fatalf("an update never fails a run: %+v", final)
	}
	if _, ok := hasNote(final.Report, "update_failed"); !ok {
		t.Errorf("notes = %+v", final.Report.Notes)
	}
	var sawUpdating bool
	for _, e := range run.events {
		if e.Kind == "log" && payload(t, e)["text"] == "Updating the agent from v0.1.0 to v0.2.0." {
			sawUpdating = true
		}
	}
	if !sawUpdating {
		t.Errorf("events = %v", kinds(run.events))
	}
	if got := strings.Join(statuses(run.statuses), ","); got != "starting,running,succeeded" {
		t.Errorf("statuses = %s", got)
	}
}

func fakeRelease(t *testing.T, version, binary string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "productos-agent", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(binary))
	_ = tw.Close()
	_ = gz.Close()
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	archive := fmt.Sprintf("productos-agent_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/download/v"+version+"/"+archive, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archive)
	mux.HandleFunc("/download/v"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sums)) })
	mux.HandleFunc("/download/v"+version+"/"+update.SignatureName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(update.Sign([]byte(sums), testReleasePrivate))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A handed-over run starts before the new process hears hello: the features come with the handoff, so a no_change is
// not mistaken for a legacy server's.
func TestHandoffKeepsTheServerFeatures(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", alreadyDoneFixture(t, "src/drafts/store.ts:1"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "")

	f := newFakeServer(t)
	f.features = allFeatures
	f.start = startMessage("run-hf", work, "productos/mob-p7-handoff-features")
	f.runs["run-hf"] = &fakeRun{status: "starting", final: make(chan protocol.StatusReport, 1)}
	if err := writeHandoff(Handoff{From: "0.0.0", To: "0.0.0-dev", WrittenAt: time.Now(), Runs: []protocol.ServerMessage{f.start}, Features: allFeatures}); err != nil {
		t.Fatal(err)
	}
	final := lastStatus(t, runDaemon(t, f, work))
	if final.Status != "succeeded" || final.Outcome != "no_change" {
		t.Errorf("final = %+v", final)
	}
}
