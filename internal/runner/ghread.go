package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What an adjust run reads from GitHub itself, with gh, before the tool starts: the log of the failed CI jobs on the
// pull request's head and the review comments since the previous run. Both feed the brief; neither can fail a run.

const (
	// ciLogLines and ciLogBytes bound what the tool is given of the CI: the last lines of each failed job, all of them
	// together within the budget.
	ciLogLines = 150
	ciLogBytes = 12 << 10
	// maxLogFetches bounds the `gh run view` calls (one per failed job).
	maxLogFetches = 4
	// maxReviewItems and reviewBodyMax bound the review comments (the newest are kept).
	maxReviewItems = 30
	reviewBodyMax  = 2000
	ghTimeout      = 45 * time.Second
)

// ghExec runs gh and returns its stdout; a non-zero exit returns the stdout it printed along with the error (gh pr
// checks exits 1 when a check failed and 8 while some are pending, with the JSON on stdout all the same).
type ghExec func(ctx context.Context, args ...string) (string, error)

// ghCommand is the real ghExec: the binary at path, never prompting, no colours.
func ghCommand(path, dir string) ghExec {
	return func(ctx context.Context, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, ghTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GH_PAGER=", "PAGER=cat")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &limitedWriter{w: &stderr, max: 16 << 10}
		err := cmd.Run()
		if err != nil {
			if msg := firstLine(stderr.String()); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		return stdout.String(), err
	}
}

// ---- CI ----

// failedJob is one failed check of the pull request's head and, for a GitHub Actions job, the tail of its log.
type failedJob struct {
	Name     string
	Workflow string
	Link     string
	RunID    string
	JobID    string
	Log      string
}

// ciLog is what the agent read of the CI.
type ciLog struct {
	Jobs []failedJob
}

// Names are the failed jobs' names, for the ci_read note.
func (c *ciLog) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Jobs))
	for _, j := range c.Jobs {
		out = append(out, j.Name)
	}
	return out
}

// WithLogs counts the jobs whose log could be read.
func (c *ciLog) WithLogs() int {
	n := 0
	if c != nil {
		for _, j := range c.Jobs {
			if j.Log != "" {
				n++
			}
		}
	}
	return n
}

// failureStates are the check states that mean "this failed" (gh pr checks' state; a cancelled check is not a
// failure to fix).
var failureStates = map[string]bool{"FAILURE": true, "ERROR": true, "TIMED_OUT": true, "ACTION_REQUIRED": true, "STARTUP_FAILURE": true, "FAIL": true}

var actionsLink = regexp.MustCompile(`/actions/runs/(\d+)(?:/job/(\d+))?`)

// parseChecks reads `gh pr checks --json name,state,link,workflow` and keeps the failed ones.
func parseChecks(raw string) ([]failedJob, error) {
	var checks []struct {
		Name     string `json:"name"`
		State    string `json:"state"`
		Link     string `json:"link"`
		Workflow string `json:"workflow"`
		Bucket   string `json:"bucket"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &checks); err != nil {
		return nil, err
	}
	var out []failedJob
	for _, c := range checks {
		if !failureStates[strings.ToUpper(c.State)] && !strings.EqualFold(c.Bucket, "fail") {
			continue
		}
		j := failedJob{Name: firstNonEmpty(strings.TrimSpace(c.Name), "check"), Workflow: c.Workflow, Link: c.Link}
		if m := actionsLink.FindStringSubmatch(c.Link); m != nil {
			j.RunID, j.JobID = m[1], m[2]
		}
		out = append(out, j)
	}
	return out, nil
}

// parseRunList reads `gh run list --json databaseId,headSha,conclusion,name,workflowName` and keeps the failed runs
// of head (every failed run when head is unknown): the fallback when gh pr checks has no --json.
func parseRunList(raw, head string) ([]failedJob, error) {
	var runs []struct {
		DatabaseID   int64  `json:"databaseId"`
		HeadSha      string `json:"headSha"`
		Conclusion   string `json:"conclusion"`
		Name         string `json:"name"`
		WorkflowName string `json:"workflowName"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &runs); err != nil {
		return nil, err
	}
	var out []failedJob
	seen := map[string]bool{}
	for _, r := range runs {
		if head != "" && !strings.HasPrefix(r.HeadSha, head) && !strings.HasPrefix(head, r.HeadSha) {
			continue
		}
		if !failureStates[strings.ToUpper(r.Conclusion)] {
			continue
		}
		name := firstNonEmpty(r.WorkflowName, r.Name, "workflow")
		if seen[name] {
			continue // the newest run of each workflow
		}
		seen[name] = true
		out = append(out, failedJob{Name: name, Workflow: r.WorkflowName, RunID: strconv.FormatInt(r.DatabaseID, 10)})
	}
	return out, nil
}

// readCI finds the failed checks of the pull request's head and reads the tail of each failed Actions job's log.
func readCI(ctx context.Context, gh ghExec, repo string, pr int, branch, head string) (*ciLog, error) {
	out, err := gh(ctx, "pr", "checks", strconv.Itoa(pr), "--repo", repo, "--json", "name,state,link,workflow")
	jobs, perr := parseChecks(out)
	if perr != nil {
		// An older gh (no --json on pr checks) or no checks at all: the runs of the branch at the head.
		listed, lerr := gh(ctx, "run", "list", "--repo", repo, "--branch", branch, "--json", "databaseId,headSha,conclusion,name,workflowName", "--limit", "20")
		if jobs, perr = parseRunList(listed, head); perr != nil {
			return nil, firstErr(err, lerr, perr)
		}
	}
	fetches := 0
	seen := map[string]bool{}
	for i := range jobs {
		j := &jobs[i]
		key := j.RunID + "/" + j.JobID
		if j.RunID == "" || seen[key] || fetches >= maxLogFetches {
			continue
		}
		seen[key] = true
		fetches++
		args := []string{"run", "view", j.RunID, "--repo", repo, "--log-failed"}
		if j.JobID != "" {
			args = []string{"run", "view", "--repo", repo, "--job", j.JobID, "--log-failed"}
		}
		raw, err := gh(ctx, args...)
		if err != nil && strings.TrimSpace(raw) == "" {
			continue
		}
		j.Log = cleanLog(raw, ciLogLines)
	}
	fitLogs(jobs, ciLogBytes)
	return &ciLog{Jobs: jobs}, nil
}

var (
	ansiCodes   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	logStamp    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z ?`)
	errorMarker = "##[error]"
)

// cleanLog turns `gh run view --log-failed` output (job \t step \t timestamp text) into the lines that matter: the
// job and step columns and the timestamps go, colours go, a run of identical lines folds to one, and the log ends at
// its last ##[error] line (what follows is the runner cleaning up). The last maxLines are kept.
func cleanLog(raw string, maxLines int) string {
	var lines []string
	lastError := -1
	prev, repeats := "", 0
	flush := func() {
		if repeats > 0 {
			lines = append(lines, fmt.Sprintf("… (the line above repeated %d more times)", repeats))
			repeats = 0
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if parts := strings.SplitN(line, "\t", 3); len(parts) == 3 {
			line = parts[2]
		}
		line = ansiCodes.ReplaceAllString(logStamp.ReplaceAllString(line, ""), "")
		line = strings.TrimRight(line, " \t\r")
		if line == prev && line != "" {
			repeats++
			continue
		}
		flush()
		prev = line
		lines = append(lines, line)
		if strings.Contains(line, errorMarker) {
			lastError = len(lines) - 1
		}
	}
	flush()
	if lastError >= 0 {
		lines = lines[:lastError+1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// fitLogs keeps the logs within budget bytes together: each log keeps its tail within an equal share.
func fitLogs(jobs []failedJob, budget int) {
	n := 0
	for _, j := range jobs {
		if j.Log != "" {
			n++
		}
	}
	if n == 0 {
		return
	}
	share := budget / n
	for i := range jobs {
		if len(jobs[i].Log) > share {
			jobs[i].Log = tailBytes(jobs[i].Log, share)
		}
	}
}

// tailBytes keeps the last n bytes, starting at a line boundary when it can.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "…\n" + strings.ToValidUTF8(cut, "")
}

// ---- reviews ----

// reviewItem is one review or review comment by someone with write access to the repository.
type reviewItem struct {
	Review      bool // a review (with its state) rather than a comment on a line
	Author      string
	Association string
	State       string
	Path        string
	Line        int
	Body        string
	At          time.Time
}

// trustedAssociations: only people the repository trusts are read (author_association).
var trustedAssociations = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

type ghUser struct {
	Login string `json:"login"`
}

// decodeArrays reads one JSON array, or several concatenated (gh api --paginate).
func decodeArrays[T any](raw string) ([]T, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	var all []T
	for {
		var page []T
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			return all, nil
		}
		if err != nil {
			return all, err
		}
		all = append(all, page...)
	}
}

// parseReviewComments reads `gh api repos/{o}/{r}/pulls/{n}/comments`: the comments on lines of the diff.
func parseReviewComments(raw string, since time.Time) ([]reviewItem, error) {
	type comment struct {
		User              ghUser    `json:"user"`
		Body              string    `json:"body"`
		Path              string    `json:"path"`
		Line              *int      `json:"line"`
		OriginalLine      *int      `json:"original_line"`
		CreatedAt         time.Time `json:"created_at"`
		AuthorAssociation string    `json:"author_association"`
	}
	comments, err := decodeArrays[comment](raw)
	if err != nil && len(comments) == 0 {
		return nil, err
	}
	var out []reviewItem
	for _, c := range comments {
		if !trustedAssociations[strings.ToUpper(c.AuthorAssociation)] || strings.TrimSpace(c.Body) == "" || !c.CreatedAt.After(since) {
			continue
		}
		it := reviewItem{Author: c.User.Login, Association: strings.ToUpper(c.AuthorAssociation), Path: c.Path, Body: strings.TrimSpace(c.Body), At: c.CreatedAt}
		if c.Line != nil {
			it.Line = *c.Line
		} else if c.OriginalLine != nil {
			it.Line = *c.OriginalLine
		}
		out = append(out, it)
	}
	return out, nil
}

// parseReviews reads `gh api repos/{o}/{r}/pulls/{n}/reviews`: submitted reviews with a body, or that requested
// changes (pending and dismissed reviews are not feedback).
func parseReviews(raw string, since time.Time) ([]reviewItem, error) {
	type review struct {
		User              ghUser    `json:"user"`
		Body              string    `json:"body"`
		State             string    `json:"state"`
		SubmittedAt       time.Time `json:"submitted_at"`
		AuthorAssociation string    `json:"author_association"`
	}
	reviews, err := decodeArrays[review](raw)
	if err != nil && len(reviews) == 0 {
		return nil, err
	}
	var out []reviewItem
	for _, r := range reviews {
		state := strings.ToUpper(r.State)
		if !trustedAssociations[strings.ToUpper(r.AuthorAssociation)] || state == "PENDING" || state == "DISMISSED" || !r.SubmittedAt.After(since) {
			continue
		}
		if strings.TrimSpace(r.Body) == "" && state != "CHANGES_REQUESTED" {
			continue
		}
		out = append(out, reviewItem{Review: true, Author: r.User.Login, Association: strings.ToUpper(r.AuthorAssociation), State: state, Body: strings.TrimSpace(r.Body), At: r.SubmittedAt})
	}
	return out, nil
}

// readReviews reads the reviews and the line comments of the pull request since `since` (zero: all of them), oldest
// first, the newest maxReviewItems kept.
func readReviews(ctx context.Context, gh ghExec, repo string, pr int, since time.Time) ([]reviewItem, error) {
	base := fmt.Sprintf("repos/%s/pulls/%d", repo, pr)
	var items []reviewItem
	var errs []error
	if raw, err := gh(ctx, "api", base+"/comments?per_page=100&sort=created&direction=desc"); err == nil {
		got, perr := parseReviewComments(raw, since)
		items = append(items, got...)
		errs = append(errs, perr)
	} else {
		errs = append(errs, err)
	}
	if raw, err := gh(ctx, "api", base+"/reviews?per_page=100"); err == nil {
		got, perr := parseReviews(raw, since)
		items = append(items, got...)
		errs = append(errs, perr)
	} else {
		errs = append(errs, err)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
	if len(items) > maxReviewItems {
		items = items[len(items)-maxReviewItems:]
	}
	if len(items) == 0 {
		return nil, errors.Join(errs...)
	}
	return items, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return errors.New("no answer")
}
