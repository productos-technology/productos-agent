// Package seed plants a product started from scratch (docs/product/SEEDS.md, docs/reference/agent-protocol.md
// "Seeding a repository"): it creates the checkout, writes the seed's files, makes the first commit and, when asked and
// `gh` is signed in, creates the GitHub repository and pushes. It never deletes anything the person had.
package seed

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/gitx"
	"github.com/productos-technology/productos-agent/internal/protocol"
)

const (
	maxEntries   = 300
	maxEntrySize = 2 << 20
	pushBudget   = 10 * time.Minute
)

// Deps is what the planter needs from its surroundings; tests fake all of it.
type Deps struct {
	// Report sends a status to the server (REST); Plant uses it for `planting`, the caller sends the final one.
	Report func(ctx context.Context, report protocol.SeedStatusReport) error
	// Archive downloads the zip of a seed of kind "zip".
	Archive func(ctx context.Context) ([]byte, error)
	// GH is the path of a signed-in gh, or "" when GitHub is not available on this machine.
	GH  string
	Log *slog.Logger
}

// failure is a coded way to stop; Plant turns it into a failed report.
type failure struct {
	code, reason string
}

func (f *failure) Error() string { return f.code + ": " + f.reason }

func fail(code, format string, args ...any) error {
	return &failure{code: code, reason: fmt.Sprintf(format, args...)}
}

// Plant runs the agent's steps and returns the final report (planted or failed). It posts only `planting`, first and
// before any disk work: the server fails the seed with no_ack when that takes more than 15 seconds.
func Plant(ctx context.Context, deps Deps, seed *protocol.Seed) protocol.SeedStatusReport {
	if deps.Log == nil {
		deps.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := deps.Report(ctx, protocol.SeedStatusReport{Status: protocol.SeedPlanting}); err != nil {
		deps.Log.Warn("cannot report planting", "seed_id", seed.ID, "error", err)
	}
	report, err := plant(ctx, deps, seed)
	if err == nil {
		return report
	}
	var f *failure
	if errors.As(err, &f) {
		return protocol.SeedStatusReport{Status: protocol.SeedFailed, ReasonCode: f.code, Reason: f.reason}
	}
	return protocol.SeedStatusReport{Status: protocol.SeedFailed, ReasonCode: protocol.SeedInternal, Reason: err.Error()}
}

func plant(ctx context.Context, deps Deps, seed *protocol.Seed) (protocol.SeedStatusReport, error) {
	dir, err := ExpandPath(seed.LocalPath)
	if err != nil {
		return protocol.SeedStatusReport{}, fmt.Errorf("resolve %q: %w", seed.LocalPath, err)
	}
	if err := checkTarget(dir); err != nil {
		return protocol.SeedStatusReport{}, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return protocol.SeedStatusReport{}, fail(protocol.SeedGitMissing, "git is not installed or not on PATH")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return protocol.SeedStatusReport{}, fmt.Errorf("create %s: %w", dir, err)
	}
	files := seed.Files
	if seed.Kind == "zip" {
		raw, err := deps.Archive(ctx)
		if err != nil {
			return protocol.SeedStatusReport{}, fmt.Errorf("download the archive: %w", err)
		}
		if err := extract(raw, dir); err != nil {
			return protocol.SeedStatusReport{}, err
		}
		if hasReadme(dir) {
			files = nil
		}
	}
	for _, f := range files {
		if err := writeFile(dir, f.Path, []byte(f.Content)); err != nil {
			return protocol.SeedStatusReport{}, fmt.Errorf("write %s into %s: %w", f.Path, dir, err)
		}
	}

	repo := &gitx.Repo{Dir: dir}
	if err := initRepo(ctx, repo); err != nil {
		return protocol.SeedStatusReport{}, err
	}
	if _, err := repo.Git(ctx, "add", "-A"); err != nil {
		return protocol.SeedStatusReport{}, fail(protocol.SeedCommitFailed, "%s", firstLine(err))
	}
	message := seed.Commit.Message
	if seed.Commit.Trailer != "" {
		message += "\n\n" + seed.Commit.Trailer
	}
	if _, err := repo.Git(ctx, "commit", "-q", "-m", message); err != nil {
		return protocol.SeedStatusReport{}, fail(protocol.SeedCommitFailed, "%s", firstLine(err))
	}
	head, err := repo.Git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return protocol.SeedStatusReport{}, fmt.Errorf("read the first commit in %s: %w", dir, err)
	}
	report := protocol.SeedStatusReport{Status: protocol.SeedPlanted, LocalPath: dir, HeadCommit: strings.TrimSpace(head), DefaultBranch: "main"}

	if seed.RepositoryFullName != "" {
		if note := createRemote(ctx, deps, repo, seed, &report); note != nil {
			report.Notes = append(report.Notes, *note)
		}
	}
	return report, nil
}

// createRemote is the optional last step: its failure never fails the seed, it becomes a note.
func createRemote(ctx context.Context, deps Deps, repo *gitx.Repo, seed *protocol.Seed, report *protocol.SeedStatusReport) *protocol.Note {
	if deps.GH == "" {
		return &protocol.Note{Code: protocol.NoteRemoteFailed, Text: "gh is not installed or not signed in on this machine"}
	}
	visibility := "--public"
	if seed.Private {
		visibility = "--private"
	}
	ctx, cancel := context.WithTimeout(ctx, pushBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, deps.GH, "repo", "create", seed.RepositoryFullName, visibility, "--source="+repo.Dir, "--remote=origin", "--push")
	cmd.Dir = repo.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		text := firstNonEmptyLine(stderr.String())
		if text == "" {
			text = firstNonEmptyLine(stdout.String())
		}
		if text == "" {
			text = err.Error()
		}
		deps.Log.Warn("gh repo create failed; the checkout stays", "seed_id", seed.ID, "error", err)
		return &protocol.Note{Code: protocol.NoteRemoteFailed, Text: text}
	}
	if out, err := repo.Git(ctx, "remote", "get-url", "origin"); err == nil {
		report.RemoteURL = strings.TrimSpace(out)
	}
	return nil
}

// ExpandPath turns "~" and "~/x" into the home directory and the result into an absolute path.
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("the path is empty")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot find the home directory: %w", err)
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

// checkTarget refuses anything but a missing path or an empty directory.
func checkTarget(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fail(protocol.SeedPathExists, "%s exists and is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fail(protocol.SeedPathExists, "%s already exists and is not empty", dir)
	}
	return nil
}

// initRepo is `git init -b main`, or `git init` + `git checkout -b main` on a git older than 2.28.
func initRepo(ctx context.Context, repo *gitx.Repo) error {
	if _, err := repo.Git(ctx, "init", "-q", "-b", "main"); err == nil {
		return nil
	}
	if _, err := repo.Git(ctx, "init", "-q"); err != nil {
		return fail(protocol.SeedInitFailed, "%s", firstLine(err))
	}
	if _, err := repo.Git(ctx, "checkout", "-q", "-b", "main"); err != nil {
		return fail(protocol.SeedInitFailed, "%s", firstLine(err))
	}
	return nil
}

// safeRel cleans a relative path from the server or an archive and refuses anything that could leave the checkout.
func safeRel(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	if name == "" || strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':') {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", false
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", false
	}
	return clean, true
}

func writeFile(root, rel string, data []byte) error {
	clean, ok := safeRel(rel)
	if !ok {
		return fmt.Errorf("unsafe path %q", rel)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

// extract unpacks a zip safely, stripping the one top-level directory that wraps every entry (what GitHub and most
// tools produce). Nothing is written until every entry has passed the checks.
func extract(raw []byte, dir string) error {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fail(protocol.SeedArchiveInvalid, "the archive is not a readable zip: %v", err)
	}
	if len(zr.File) > maxEntries {
		return fail(protocol.SeedArchiveInvalid, "the archive has %d entries; the limit is %d", len(zr.File), maxEntries)
	}
	type entry struct {
		file  *zip.File
		parts []string
		isDir bool
	}
	var entries []entry
	for _, f := range zr.File {
		if f.Mode()&os.ModeSymlink != 0 {
			return fail(protocol.SeedArchiveInvalid, "the archive contains a symbolic link (%s)", f.Name)
		}
		clean, ok := safeRel(f.Name)
		if !ok {
			return fail(protocol.SeedArchiveInvalid, "the archive contains an unsafe path (%s)", f.Name)
		}
		isDir := f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/")
		if !isDir && f.UncompressedSize64 > maxEntrySize {
			return fail(protocol.SeedArchiveInvalid, "%s is larger than %d MB", f.Name, maxEntrySize>>20)
		}
		entries = append(entries, entry{file: f, parts: strings.Split(clean, "/"), isDir: isDir})
	}
	strip := len(entries) > 0
	nested := false
	top := ""
	for i, e := range entries {
		if i == 0 {
			top = e.parts[0]
		}
		if e.parts[0] != top {
			strip = false
			break
		}
		if len(e.parts) > 1 || e.isDir {
			nested = true
		}
	}
	strip = strip && nested
	for _, e := range entries {
		parts := e.parts
		if strip {
			parts = parts[1:]
		}
		if len(parts) == 0 {
			continue
		}
		target := filepath.Join(dir, filepath.Join(parts...))
		if e.isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			continue
		}
		if err := copyEntry(e.file, target); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fail(protocol.SeedArchiveInvalid, "cannot read %s: %v", f.Name, err)
	}
	defer rc.Close()
	// The declared size can lie: the limit is enforced on what actually comes out.
	data, err := io.ReadAll(io.LimitReader(rc, maxEntrySize+1))
	if err != nil {
		return fail(protocol.SeedArchiveInvalid, "cannot read %s: %v", f.Name, err)
	}
	if len(data) > maxEntrySize {
		return fail(protocol.SeedArchiveInvalid, "%s is larger than %d MB", f.Name, maxEntrySize>>20)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	mode := os.FileMode(0o644)
	if f.Mode()&0o100 != 0 {
		mode = 0o755
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

func hasReadme(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), "README.md") {
			return true
		}
	}
	return false
}

// firstLine is the first non-empty line of what git said on stderr, or the error itself.
func firstLine(err error) string {
	var ge *gitx.GitError
	if errors.As(err, &ge) {
		if l := firstNonEmptyLine(ge.Stderr); l != "" {
			return l
		}
	}
	return err.Error()
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
