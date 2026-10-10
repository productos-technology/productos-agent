package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hamdan85/productos-agent/internal/api"
	"github.com/Hamdan85/productos-agent/internal/config"
	"github.com/Hamdan85/productos-agent/internal/logx"
	"github.com/Hamdan85/productos-agent/internal/protocol"
)

// The test binary doubles as the fake coding tool: PRODUCTOS_AGENT_FAKE_TOOL=1 makes it stream a fixture.
func TestMain(m *testing.M) {
	if os.Getenv("PRODUCTOS_AGENT_FAKE_TOOL") == "1" {
		fakeTool()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeTool() {
	prompt, _ := io.ReadAll(os.Stdin)
	resumed := false
	for _, a := range os.Args {
		if a == "--resume" || a == "resume" {
			resumed = true
		}
	}
	if !resumed && !strings.Contains(string(prompt), "ProductOS Agent") {
		fmt.Fprintln(os.Stderr, "fake tool: prompt lacks the preamble")
		os.Exit(2)
	}
	if resumed && !strings.Contains(string(prompt), "The person answered your question") {
		fmt.Fprintln(os.Stderr, "fake tool: resumed prompt lacks the answer")
		os.Exit(2)
	}
	fixture := os.Getenv("PRODUCTOS_AGENT_FAKE_FIXTURE")
	edits := os.Getenv("PRODUCTOS_AGENT_FAKE_EDITS")
	if resumed {
		fixture = os.Getenv("PRODUCTOS_AGENT_FAKE_FIXTURE_RESUMED")
		edits = os.Getenv("PRODUCTOS_AGENT_FAKE_EDITS_RESUMED")
	}
	if edits != "" {
		for _, e := range strings.Split(edits, ";") {
			kv := strings.SplitN(e, "=", 2)
			if len(kv) != 2 {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(kv[0]), 0o755)
			_ = os.WriteFile(kv[0], []byte(strings.ReplaceAll(kv[1], "\\n", "\n")), 0o644)
		}
	}
	f, err := os.Open(fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake tool:", err)
		os.Exit(2)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		if sc.Text() == "#SLEEP" {
			time.Sleep(30 * time.Second)
			continue
		}
		fmt.Println(sc.Text())
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- fake ProductOS ----

type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	devices  map[string]map[string]any
	links    []api.LinkRepositoryInput
	runs     map[string]*fakeRun
	heartbts int
	// what the test injects
	start        protocol.ServerMessage
	answerText   string
	cancelAt     string // event kind that triggers run.cancel
	rejectRemote bool
}

type fakeRun struct {
	status   string
	events   []protocol.Event
	statuses []protocol.StatusReport
	final    chan protocol.StatusReport
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, devices: map[string]map[string]any{}, runs: map[string]*fakeRun{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/devices", f.auth(f.registerDevice))
	mux.HandleFunc("/api/v1/agent/devices/", f.auth(f.deviceRoutes))
	mux.HandleFunc("/api/v1/agent/runs", f.auth(f.listRuns))
	mux.HandleFunc("/api/v1/agent/runs/", f.auth(f.runRoutes))
	mux.HandleFunc("/cable", f.cable)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer poa_test" {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", scope="agent"`)
			writeErr(w, 401, "unauthenticated", "")
			return
		}
		next(w, r)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string, details ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := map[string]any{"code": code, "message": msg}
	if len(details) > 0 {
		e["details"] = details[0]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeServer) deviceView(id string) map[string]any {
	d := f.devices[id]
	repos := []any{}
	for i, l := range f.links {
		repos = append(repos, map[string]any{"id": fmt.Sprintf("link-%d", i), "repository_id": "repo-1", "full_name": "acme/mobile", "default_branch": l.DefaultBranch, "local_path": l.LocalPath, "remote_url": l.RemoteURL, "workspace": map[string]any{"id": "ws-1", "slug": "acme", "name": "Acme"}, "linked_at": "2026-10-10T00:00:00Z"})
	}
	return map[string]any{"id": id, "name": d["name"], "platform": d["platform"], "arch": d["arch"], "agent_version": d["agent_version"], "tools": d["tools"], "online": true, "last_seen_at": nil, "revoked_at": nil, "repositories": repos, "created_at": "2026-10-10T00:00:00Z"}
}

func (f *fakeServer) registerDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 404, "not_found", "")
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	for _, k := range []string{"name", "platform", "arch", "agent_version"} {
		if s, _ := in[k].(string); s == "" {
			writeErr(w, 422, "validation_failed", k+" required")
			return
		}
	}
	f.mu.Lock()
	id := fmt.Sprintf("dev-%d", len(f.devices)+1)
	f.devices[id] = in
	view := f.deviceView(id)
	f.mu.Unlock()
	writeJSON(w, 201, view)
}

func (f *fakeServer) deviceRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/agent/devices/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.devices[id]; !ok {
		writeErr(w, 404, "not_found", "")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodPatch:
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if t, ok := in["tools"]; ok {
			f.devices[id]["tools"] = t
		}
		f.heartbts++
		writeJSON(w, 200, f.deviceView(id))
	case len(parts) == 1 && r.Method == http.MethodDelete:
		delete(f.devices, id)
		w.WriteHeader(204)
	case len(parts) == 2 && parts[1] == "repositories" && r.Method == http.MethodPost:
		var in api.LinkRepositoryInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.rejectRemote {
			writeErr(w, 422, "unknown_remote", "unknown remote", map[string]any{"remote": nil, "expected": []string{"github.com/acme/mobile", "github.com/acme/web"}})
			return
		}
		f.links = append(f.links, in)
		writeJSON(w, 201, f.deviceView(id)["repositories"].([]any)[len(f.links)-1])
	default:
		writeErr(w, 404, "not_found", "")
	}
}

func (f *fakeServer) listRuns(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []any{}
	for id, run := range f.runs {
		if run.status != "succeeded" && run.status != "failed" && run.status != "cancelled" {
			items = append(items, map[string]any{"id": id, "status": run.status, "tool": "claude", "device": map[string]any{"id": "dev-1"}, "repository": map[string]any{"id": "repo-1"}, "last_seq": len(run.events) + 1})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// REST fallbacks of events and status: used only when the socket is down, kept honest anyway.
func (f *fakeServer) runRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/agent/runs/")
	parts := strings.Split(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[parts[0]]
	if !ok {
		writeErr(w, 404, "not_found", "")
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "events":
		var in struct{ Events []protocol.Event }
		_ = json.NewDecoder(r.Body).Decode(&in)
		run.events = append(run.events, in.Events...)
		writeJSON(w, 201, map[string]any{"last_seq": len(run.events) + 1, "events": []any{}})
	case len(parts) == 2 && parts[1] == "status":
		var in protocol.StatusReport
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.applyStatus(run, in)
		writeJSON(w, 200, map[string]any{"id": parts[0], "status": run.status})
	default:
		writeJSON(w, 200, map[string]any{"run": map[string]any{"id": parts[0], "status": run.status}, "events": []any{}})
	}
}

func (f *fakeServer) applyStatus(run *fakeRun, in protocol.StatusReport) {
	if run.status == "succeeded" || run.status == "failed" || run.status == "cancelled" {
		return
	}
	run.statuses = append(run.statuses, in)
	run.status = in.Status
	if in.Status == "succeeded" || in.Status == "failed" || in.Status == "cancelled" {
		select {
		case run.final <- in:
		default:
		}
	}
}

// cable: the Action Cable server side, enough for one AgentChannel subscription.
func (f *fakeServer) cable(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != "poa_test" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Header.Get("Origin") != f.srv.URL {
		http.Error(w, "origin", 403)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ctx := r.Context()
	send := func(v any) {
		raw, _ := json.Marshal(v)
		_ = c.Write(ctx, websocket.MessageText, raw)
	}
	send(map[string]any{"type": "welcome"})
	var identifier string
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var frame struct {
			Command    string `json:"command"`
			Identifier string `json:"identifier"`
			Data       string `json:"data"`
		}
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.Command {
		case "subscribe":
			var ident struct {
				Channel  string `json:"channel"`
				DeviceID string `json:"device_id"`
			}
			_ = json.Unmarshal([]byte(frame.Identifier), &ident)
			f.mu.Lock()
			_, known := f.devices[ident.DeviceID]
			f.mu.Unlock()
			if ident.Channel != "AgentChannel" || !known {
				send(map[string]any{"type": "reject_subscription", "identifier": frame.Identifier})
				continue
			}
			identifier = frame.Identifier
			send(map[string]any{"type": "confirm_subscription", "identifier": identifier})
			send(map[string]any{"identifier": identifier, "message": map[string]any{"type": "hello", "protocol_version": 1, "device_id": ident.DeviceID}})
			// The person pressed Run.
			f.mu.Lock()
			start := f.start
			f.runs[start.RunID] = &fakeRun{status: "queued", final: make(chan protocol.StatusReport, 1)}
			f.mu.Unlock()
			raw, _ := json.Marshal(start)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			send(map[string]any{"identifier": identifier, "message": m})
		case "message":
			var d map[string]any
			_ = json.Unmarshal([]byte(frame.Data), &d)
			action, _ := d["action"].(string)
			runID, _ := d["run_id"].(string)
			reply := func(msg map[string]any) { send(map[string]any{"identifier": identifier, "message": msg}) }
			switch action {
			case "heartbeat":
				f.mu.Lock()
				f.heartbts++
				f.mu.Unlock()
			case "run_event":
				f.mu.Lock()
				run := f.runs[runID]
				if run == nil {
					f.mu.Unlock()
					reply(map[string]any{"type": "error", "run_id": runID, "code": "not_found", "message": "no such run"})
					continue
				}
				if run.status == "succeeded" || run.status == "failed" || run.status == "cancelled" {
					f.mu.Unlock()
					reply(map[string]any{"type": "error", "run_id": runID, "code": "run_finished", "message": "finished"})
					continue
				}
				raw, _ := json.Marshal(d["events"])
				var events []protocol.Event
				_ = json.Unmarshal(raw, &events)
				var questionSeq int
				cancelNow := false
				for _, ev := range events {
					run.events = append(run.events, ev)
					if ev.Kind == "question" {
						questionSeq = len(run.events) + 1
						run.status = "waiting_input"
					}
					if f.cancelAt != "" && ev.Kind == f.cancelAt {
						cancelNow = true
					}
				}
				lastSeq := len(run.events) + 1
				answer := f.answerText
				f.mu.Unlock()
				reply(map[string]any{"type": "ack", "run_id": runID, "last_seq": lastSeq})
				if questionSeq > 0 && answer != "" {
					time.Sleep(50 * time.Millisecond)
					f.mu.Lock()
					run.events = append(run.events, protocol.Event{Kind: "answer", Payload: map[string]any{"seq": questionSeq, "text": answer}})
					run.status = "running"
					f.mu.Unlock()
					reply(map[string]any{"type": "run.answer", "protocol_version": 1, "run_id": runID, "seq": questionSeq, "answer_seq": questionSeq + 1, "text": answer})
				}
				if cancelNow {
					f.mu.Lock()
					f.applyStatus(run, protocol.StatusReport{Status: "cancelled", Reason: "cancelled_by_user"})
					f.mu.Unlock()
					reply(map[string]any{"type": "run.cancel", "protocol_version": 1, "run_id": runID, "reason": "cancelled_by_user"})
				}
			case "run_status":
				raw, _ := json.Marshal(d)
				var report protocol.StatusReport
				_ = json.Unmarshal(raw, &report)
				f.mu.Lock()
				run := f.runs[runID]
				if run != nil {
					f.applyStatus(run, report)
				}
				f.mu.Unlock()
				reply(map[string]any{"type": "ack", "run_id": runID, "status": report.Status})
			}
		}
	}
}

// ---- helpers ----

type staticTokens struct{}

func (staticTokens) Token(context.Context) (string, error)    { return "poa_test", nil }
func (staticTokens) Invalidate(context.Context, string) error { return nil }

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeCheckout builds a bare origin and a clone with one commit on main.
func makeCheckout(t *testing.T) (origin, work string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	work = filepath.Join(root, "mobile")
	git(t, root, "init", "--bare", "-b", "main", origin)
	git(t, root, "clone", "-q", origin, work)
	git(t, work, "config", "user.name", "t")
	git(t, work, "config", "user.email", "t@example.com")
	git(t, work, "checkout", "-q", "-b", "main")
	_ = os.MkdirAll(filepath.Join(work, "src", "drafts"), 0o755)
	_ = os.WriteFile(filepath.Join(work, "src", "drafts", "store.ts"), []byte("export const store = new Map();\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, "README.md"), []byte("# Mobile\n"), 0o644)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "init")
	git(t, work, "push", "-q", "-u", "origin", "main")
	git(t, work, "remote", "set-head", "origin", "main")
	return origin, work
}

func fixture(t *testing.T, name string) string {
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func startMessage(runID, work, branch string) protocol.ServerMessage {
	open := true
	return protocol.ServerMessage{
		Type: "run.start", ProtocolVersion: 1, RunID: runID, Tool: "claude",
		Subject:          &protocol.Subject{Kind: "pain", Key: "MOB-P7", Name: "Offline drafts are lost", URL: "https://productos.test/app/acme/MOB-P7"},
		Workspace:        &protocol.Workspace{ID: "ws-1", Slug: "acme", Name: "Acme"},
		Repository:       &protocol.RunRepository{ID: "repo-1", FullName: "acme/mobile", LocalPath: work, RemoteURL: "git@github.com:acme/mobile.git", DefaultBranch: "main"},
		Branch:           branch,
		ChangeDefinition: &protocol.ChangeDefinition{Markdown: "# MOB-P7 Offline drafts are lost\n\nKeep drafts across reloads.\n", JSON: json.RawMessage(`{"format_version":2}`)},
		Options:          &protocol.RunOptions{CommitTrailer: "ProductOS: MOB-P7", OpenPR: open, RunTests: true},
	}
}

func isolate(t *testing.T) {
	t.Setenv("PRODUCTOS_AGENT_CONFIG_DIR", t.TempDir())
	t.Setenv("PRODUCTOS_AGENT_STATE_DIR", t.TempDir())
	t.Setenv("PRODUCTOS_AGENT_NO_KEYRING", "1")
	t.Setenv("PRODUCTOS_AGENT_FAKE_TOOL", "1")
	t.Setenv("PATH", filepath.Dir(os.Args[0])+string(os.PathListSeparator)+os.Getenv("PATH")) // no gh in tests
	t.Setenv("GH_TOKEN", "")
}

func runDaemon(t *testing.T, f *fakeServer, work string) *fakeRun {
	t.Helper()
	cfg := &config.Config{Server: f.srv.URL}
	client := api.New(f.srv.URL, staticTokens{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// register -> link, through the REST client the CLI uses
	device, err := client.RegisterDevice(ctx, api.RegisterDeviceInput{Name: "test-box", Platform: "linux", Arch: "amd64", AgentVersion: "0.0.0-test", Tools: protocol.Tools{"claude": {Available: true, Version: "2.1.292"}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.DeviceID = device.ID
	link, err := client.LinkRepository(ctx, device.ID, api.LinkRepositoryInput{LocalPath: work, RemoteURL: "git@github.com:acme/mobile.git", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if link.FullName != "acme/mobile" || link.LocalPath != work {
		t.Fatalf("link = %+v", link)
	}

	d := &Daemon{Config: cfg, API: client, Tokens: staticTokens{}, Log: logx.Discard(), Once: true, ToolPath: os.Args[0],
		Detect: func(context.Context) protocol.Tools {
			return protocol.Tools{"claude": {Available: true, Version: "2.1.292"}}
		}}
	if err := d.Run(ctx); err != nil {
		t.Fatalf("daemon: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runs[f.start.RunID]
	if run == nil {
		t.Fatal("the run never reached the server")
	}
	return run
}

func kinds(events []protocol.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func statuses(reports []protocol.StatusReport) []string {
	out := make([]string, len(reports))
	for i, r := range reports {
		out[i] = r.Status
	}
	return out
}

func payload(t *testing.T, e protocol.Event) map[string]any {
	raw, _ := json.Marshal(e.Payload)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ---- the scenarios ----

func TestRegisterLinkRunEventsAndSuccess(t *testing.T) {
	isolate(t)
	origin, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n;src/drafts/reload.ts=export const reload = () => {};\\n")

	f := newFakeServer(t)
	f.start = startMessage("run-1", work, "productos/mob-p7-offline-drafts-are-lost")
	run := runDaemon(t, f, work)

	got := statuses(run.statuses)
	want := []string{"starting", "running", "succeeded"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses = %v, want %v (final %+v)", got, want, run.statuses[len(run.statuses)-1])
	}
	final := run.statuses[len(run.statuses)-1]
	if final.Branch != "productos/mob-p7-offline-drafts-are-lost" {
		t.Errorf("branch = %q", final.Branch)
	}
	if len(final.HeadCommit) < 7 {
		t.Errorf("head_commit = %q", final.HeadCommit)
	}
	if !strings.Contains(final.Summary, "2 file(s) changed") || !strings.Contains(final.Summary, "I created `notes.txt`") {
		t.Errorf("summary = %q", final.Summary)
	}
	if final.PRURL != "" {
		t.Errorf("no gh in the test: pr_url should be empty, got %q", final.PRURL)
	}

	// The branch landed on origin with the trailer.
	if head := git(t, origin, "rev-parse", "refs/heads/"+final.Branch); head != final.HeadCommit {
		t.Errorf("origin head %s != reported %s", head, final.HeadCommit)
	}
	msg := git(t, origin, "log", "-1", "--format=%B", final.HeadCommit)
	if !strings.Contains(msg, "MOB-P7: Offline drafts are lost") || !strings.Contains(msg, "ProductOS: MOB-P7") {
		t.Errorf("commit message = %q", msg)
	}

	// Events: the drawer sees the tool's work.
	k := kinds(run.events)
	counts := map[string]int{}
	for _, kind := range k {
		counts[kind]++
	}
	if counts["tool_use"] < 2 || counts["message"] < 1 || counts["file_change"] < 2 || counts["log"] < 2 {
		t.Errorf("event kinds = %v", k)
	}
	var sawCreated, sawModified bool
	for _, e := range run.events {
		if e.Kind != "file_change" {
			continue
		}
		p := payload(t, e)
		switch p["path"] {
		case "src/drafts/reload.ts":
			sawCreated = p["action"] == "created" && p["additions"] == float64(1)
		case "src/drafts/store.ts":
			sawModified = p["action"] == "modified" && p["additions"] == float64(1) && p["deletions"] == float64(1)
		}
	}
	if !sawCreated || !sawModified {
		t.Errorf("file_change payloads wrong: created=%v modified=%v", sawCreated, sawModified)
	}
	if f.heartbts < 1 {
		t.Errorf("expected at least one heartbeat (register PATCH or socket), got %d", f.heartbts)
	}
	if len(f.links) != 1 {
		t.Errorf("links = %d", len(f.links))
	}
}

func TestQuestionAnswerResumeAndTests(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/question_and_tests.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "")
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE_RESUMED", fixture(t, "claude/resumed_with_tests.jsonl"))
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS_RESUMED", "src/drafts/store.ts=export const store = openSqlite('drafts');\\n;src/drafts/reload.ts=export const reload = () => {};\\n")

	f := newFakeServer(t)
	f.start = startMessage("run-2", work, "") // no branch from the server: the fallback name is used
	f.answerText = "sqlite"
	run := runDaemon(t, f, work)

	got := statuses(run.statuses)
	if got[len(got)-1] != "succeeded" {
		t.Fatalf("statuses = %v, final %+v", got, run.statuses[len(run.statuses)-1])
	}
	if b := run.statuses[len(run.statuses)-1].Branch; b != "productos/mob-p7-offline-drafts-are-lost" {
		t.Errorf("fallback branch = %q", b)
	}
	k := kinds(run.events)
	var qIndex, aIndex, userIndex = -1, -1, -1
	tests := []map[string]any{}
	for i, e := range run.events {
		switch e.Kind {
		case "question":
			qIndex = i
			p := payload(t, e)
			if p["text"] != "Which storage should keep the drafts offline?" {
				t.Errorf("question = %v", p)
			}
			if opts, _ := p["options"].([]any); len(opts) != 2 {
				t.Errorf("options = %v", p["options"])
			}
		case "answer":
			aIndex = i
		case "message":
			if payload(t, e)["role"] == "user" {
				userIndex = i
			}
		case "test":
			tests = append(tests, payload(t, e))
		}
	}
	if qIndex < 0 || aIndex < qIndex || userIndex < aIndex {
		t.Errorf("order wrong: question=%d answer=%d user-message=%d kinds=%v", qIndex, aIndex, userIndex, k)
	}
	if len(tests) != 2 || tests[0]["passed"] != false || tests[1]["passed"] != true || tests[0]["command"] != "npm test -- drafts" {
		t.Errorf("tests = %v", tests)
	}
	if s := run.statuses[len(run.statuses)-1].Summary; !strings.Contains(s, "Test runs: 1 passed, 1 failed") {
		t.Errorf("summary = %q", s)
	}
}

func TestCancelKillsTheTool(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	dir := t.TempDir()
	fix := filepath.Join(dir, "slow.jsonl")
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s-cancel"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Starting the long task."}]},"session_id":"s-cancel"}`,
		"#SLEEP",
		`{"type":"result","subtype":"success","is_error":false,"result":"never reached","session_id":"s-cancel"}`,
	}
	_ = os.WriteFile(fix, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fix)
	t.Setenv("PRODUCTOS_AGENT_FAKE_EDITS", "")

	f := newFakeServer(t)
	f.start = startMessage("run-3", work, "productos/mob-p7-cancel")
	f.cancelAt = "message"
	started := time.Now()
	run := runDaemon(t, f, work)
	if time.Since(started) > 25*time.Second {
		t.Errorf("cancel took %s: the process group was not killed", time.Since(started))
	}
	got := statuses(run.statuses)
	if got[len(got)-1] != "cancelled" {
		t.Errorf("statuses = %v", got)
	}
}

func TestDirtyCheckoutFails(t *testing.T) {
	isolate(t)
	_, work := makeCheckout(t)
	_ = os.WriteFile(filepath.Join(work, "dirty.txt"), []byte("x"), 0o644)
	t.Setenv("PRODUCTOS_AGENT_FAKE_FIXTURE", fixture(t, "claude/simple_run.jsonl"))
	f := newFakeServer(t)
	f.start = startMessage("run-4", work, "productos/mob-p7-dirty")
	run := runDaemon(t, f, work)
	got := statuses(run.statuses)
	if strings.Join(got, ",") != "starting,failed" {
		t.Fatalf("statuses = %v", got)
	}
	if r := run.statuses[1].Reason; !strings.Contains(r, "uncommitted changes") {
		t.Errorf("reason = %q", r)
	}
}

func TestUnknownRemoteIsExplained(t *testing.T) {
	isolate(t)
	f := newFakeServer(t)
	f.rejectRemote = true
	client := api.New(f.srv.URL, staticTokens{})
	ctx := context.Background()
	device, err := client.RegisterDevice(ctx, api.RegisterDeviceInput{Name: "x", Platform: "linux", Arch: "amd64", AgentVersion: "0"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LinkRepository(ctx, device.ID, api.LinkRepositoryInput{LocalPath: "/x", RemoteURL: "git@gitlab.com:other/app.git", DefaultBranch: "main"})
	if !api.IsCode(err, "unknown_remote") {
		t.Fatalf("err = %v", err)
	}
	var apiErr *api.Error
	if !strings.Contains(fmt.Sprint(err), "unknown_remote") || !asAPI(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	var details api.UnknownRemote
	if err := json.Unmarshal(apiErr.Details, &details); err != nil || len(details.Expected) != 2 || details.Remote != nil {
		t.Errorf("details = %+v (%v)", details, err)
	}
}

func asAPI(err error, target **api.Error) bool {
	e, ok := err.(*api.Error)
	if ok {
		*target = e
	}
	return ok
}
