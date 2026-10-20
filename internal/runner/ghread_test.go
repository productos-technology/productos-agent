package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ghFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "gh", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseChecksKeepsTheFailedOnes(t *testing.T) {
	jobs, err := parseChecks(ghFixture(t, "pr_checks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range jobs {
		names = append(names, j.Name)
	}
	if strings.Join(names, ",") != "Tests (unit),Build,external/security" {
		t.Fatalf("failed = %v (cancelled, skipped and passed checks are not failures)", names)
	}
	if jobs[0].RunID != "39500140593" || jobs[0].JobID != "118895248812" || jobs[0].Workflow != "CI" {
		t.Errorf("actions ids = %+v", jobs[0])
	}
	if jobs[2].RunID != "" || jobs[2].JobID != "" {
		t.Errorf("a check outside GitHub Actions has no run: %+v", jobs[2])
	}
	if _, err := parseChecks("no checks reported on the 'x' branch"); err == nil {
		t.Error("text is not JSON")
	}
}

func TestParseRunListFiltersTheHead(t *testing.T) {
	jobs, err := parseRunList(ghFixture(t, "run_list.json"), "4f2a1c3")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].RunID != "39490229066" || jobs[0].Name != "CI" {
		t.Errorf("jobs = %+v", jobs)
	}
	all, _ := parseRunList(ghFixture(t, "run_list.json"), "")
	if len(all) != 1 {
		t.Errorf("without a head, the newest failed run of each workflow: %+v", all)
	}
}

func TestCleanLogEndsAtTheErrorAndDropsTheNoise(t *testing.T) {
	log := cleanLog(ghFixture(t, "run_log_failed.txt"), ciLogLines)
	for _, want := range []string{"FAIL  src/drafts/store.test.ts", "AssertionError: expected undefined to equal 'hello'", "expect(draft).toEqual('hello');", "##[error]Process completed with exit code 1.", "(the line above repeated 29 more times)"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	for _, unwanted := range []string{"Cleaning up orphan processes", "upload-artifact", "Tests (unit)\t", "UNKNOWN STEP", "2026-10-20T", "\x1b["} {
		if strings.Contains(log, unwanted) {
			t.Errorf("log keeps %q", unwanted)
		}
	}
	if !strings.HasSuffix(log, "##[error]Process completed with exit code 1.") {
		t.Errorf("the log ends at its last error line: %q", log[max(0, len(log)-80):])
	}
	short := cleanLog(ghFixture(t, "run_log_failed.txt"), 5)
	if lines := strings.Split(short, "\n"); len(lines) != 5 || lines[4] != "##[error]Process completed with exit code 1." {
		t.Errorf("last 5 lines = %q", lines)
	}
	plain := cleanLog("first\nsecond\n", 150)
	if plain != "first\nsecond" {
		t.Errorf("a log without columns or errors stays as it is: %q", plain)
	}
}

func TestFitLogsSharesTheBudget(t *testing.T) {
	big := strings.Repeat("0123456789abcdef\n", 2000) // 34 KB
	jobs := []failedJob{{Name: "a", Log: big}, {Name: "b", Log: "short"}, {Name: "c"}}
	fitLogs(jobs, ciLogBytes)
	total := 0
	for _, j := range jobs {
		total += len(j.Log)
	}
	if total > ciLogBytes+16 || jobs[1].Log != "short" || !strings.HasPrefix(jobs[0].Log, "…\n0123") {
		t.Errorf("total = %d, a = %q…", total, jobs[0].Log[:20])
	}
}

func TestParseReviewCommentsAndReviews(t *testing.T) {
	since := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	comments, err := parseReviewComments(ghFixture(t, "pull_comments.json"), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || comments[0].Author != "pat" || comments[0].Line != 42 || comments[1].Author != "lee" || comments[1].Line != 12 || comments[1].Path != "src/store.ts" {
		t.Fatalf("comments = %+v (no NONE author, nothing before the cut)", comments)
	}
	reviews, err := parseReviews(ghFixture(t, "pull_reviews.json"), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 2 || reviews[0].Author != "pat" || reviews[0].State != "CHANGES_REQUESTED" || reviews[1].Author != "lee" || reviews[1].Body != "" {
		t.Fatalf("reviews = %+v (no empty comment-only review, no pending, dismissed or NONE)", reviews)
	}
	all, _ := parseReviewComments(ghFixture(t, "pull_comments.json"), time.Time{})
	if len(all) != 3 {
		t.Errorf("without a cut every trusted comment: %d", len(all))
	}
	paged, err := parseReviews(`[{"user":{"login":"a"},"body":"x","state":"COMMENTED","submitted_at":"2026-10-19T10:00:00Z","author_association":"OWNER"}][{"user":{"login":"b"},"body":"y","state":"APPROVED","submitted_at":"2026-10-19T11:00:00Z","author_association":"MEMBER"}]`, since)
	if err != nil || len(paged) != 2 {
		t.Errorf("concatenated pages = %+v (%v)", paged, err)
	}
}

// fakeGH answers gh calls from fixtures keyed by the first words of the arguments.
type fakeGH struct {
	t       *testing.T
	answers map[string]struct {
		out string
		err error
	}
	calls []string
}

func (f *fakeGH) on(prefix, out string, err error) {
	if f.answers == nil {
		f.answers = map[string]struct {
			out string
			err error
		}{}
	}
	f.answers[prefix] = struct {
		out string
		err error
	}{out, err}
}

func (f *fakeGH) exec(_ context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	best := ""
	for prefix := range f.answers {
		if strings.HasPrefix(call, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return "", errors.New("unexpected gh call: " + call)
	}
	a := f.answers[best]
	return a.out, a.err
}

func TestReadCIReadsTheFailedJobsLogs(t *testing.T) {
	gh := &fakeGH{t: t}
	// gh pr checks exits 1 when a check failed: the JSON is on stdout all the same.
	gh.on("pr checks 25 --repo acme/mobile --json", ghFixture(t, "pr_checks.json"), errors.New("exit status 1"))
	gh.on("run view --repo acme/mobile --job 118895248812 --log-failed", ghFixture(t, "run_log_failed.txt"), nil)
	gh.on("run view --repo acme/mobile --job 118895248908 --log-failed", "", errors.New("exit status 1: log not found"))
	ci, err := readCI(context.Background(), gh.exec, "acme/mobile", 25, "productos/mob-c4-x", "4f2a1c3")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ci.Names(), ",") != "Tests (unit),Build,external/security" || ci.WithLogs() != 1 {
		t.Fatalf("ci = %+v", ci)
	}
	if !strings.Contains(ci.Jobs[0].Log, "AssertionError") {
		t.Errorf("log = %q", ci.Jobs[0].Log)
	}
	if len(gh.calls) != 3 {
		t.Errorf("calls = %v (one pr checks, one log per Actions job)", gh.calls)
	}
}

func TestReadCIFallsBackToTheRunList(t *testing.T) {
	gh := &fakeGH{t: t}
	gh.on("pr checks", "", errors.New("unknown flag: --json"))
	gh.on("run list --repo acme/mobile --branch productos/mob-c4-x", ghFixture(t, "run_list.json"), nil)
	gh.on("run view 39490229066 --repo acme/mobile --log-failed", ghFixture(t, "run_log_failed.txt"), nil)
	ci, err := readCI(context.Background(), gh.exec, "acme/mobile", 25, "productos/mob-c4-x", "4f2a1c3de2bd")
	if err != nil {
		t.Fatal(err)
	}
	if len(ci.Jobs) != 1 || ci.Jobs[0].Name != "CI" || ci.WithLogs() != 1 {
		t.Errorf("ci = %+v", ci)
	}
}

func TestReadReviewsMergesAndOrders(t *testing.T) {
	gh := &fakeGH{t: t}
	gh.on("api repos/acme/mobile/pulls/25/comments", ghFixture(t, "pull_comments.json"), nil)
	gh.on("api repos/acme/mobile/pulls/25/reviews", ghFixture(t, "pull_reviews.json"), nil)
	items, err := readReviews(context.Background(), gh.exec, "acme/mobile", 25, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, it := range items {
		order = append(order, it.Author)
	}
	if strings.Join(order, ",") != "pat,pat,lee,lee" {
		t.Errorf("order = %v (oldest first)", order)
	}

	failing := &fakeGH{t: t}
	failing.on("api", "", errors.New("HTTP 404"))
	if _, err := readReviews(context.Background(), failing.exec, "acme/mobile", 25, time.Time{}); err == nil {
		t.Error("both endpoints failing is an error (the caller warns)")
	}
}

func TestResumeFailedOnlyWhenTheSessionNeverStarted(t *testing.T) {
	notFound := toolOutcome{failed: true, failCode: codeToolError, result: "No conversation found with session ID: s-1"}
	if why, ok := resumeFailed(notFound, nil); !ok || !strings.Contains(why, "No conversation found") {
		t.Errorf("not found: %q %v", why, ok)
	}
	if _, ok := resumeFailed(toolOutcome{}, &runError{codeToolCrashed, "codex exited (exit status 1): thread not found"}); !ok {
		t.Error("a crash before any output falls back")
	}
	if _, ok := resumeFailed(toolOutcome{worked: true}, &runError{codeToolCrashed, "boom"}); ok {
		t.Error("a session that worked and then crashed is the run's failure")
	}
	if _, ok := resumeFailed(toolOutcome{failed: true, failCode: codeToolAuth, result: "Invalid API key"}, nil); ok {
		t.Error("a signed-out tool would fail a fresh session too")
	}
	if _, ok := resumeFailed(toolOutcome{result: "done"}, nil); ok {
		t.Error("a resumed session that finished is fine")
	}
}
