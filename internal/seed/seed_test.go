package seed

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/productos-technology/productos-agent/internal/protocol"
)

type fakeDeps struct {
	reports []protocol.SeedStatusReport
	archive []byte
	gh      string
}

func (f *fakeDeps) deps() Deps {
	return Deps{
		Report: func(_ context.Context, r protocol.SeedStatusReport) error {
			f.reports = append(f.reports, r)
			return nil
		},
		Archive: func(context.Context) ([]byte, error) { return f.archive, nil },
		GH:      f.gh,
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func newSeed(kind, path string) *protocol.Seed {
	s := &protocol.Seed{ID: "s1", Kind: kind, LocalPath: path, Status: "pending",
		Files: []protocol.SeedFile{{Path: "README.md", Content: "# Acme\n"}, {Path: "docs/PRODUCT.md", Content: "Build a thing\n"}}}
	s.Commit.Message = "Start Acme"
	s.Commit.Trailer = "ProductOS: MOB"
	return s
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func makeZip(t *testing.T, entries map[string]string, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if len(order) == 0 {
		for name := range entries {
			order = append(order, name)
		}
	}
	for _, name := range order {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(entries[name]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPromptSeedCreatesFilesAndOneCommit(t *testing.T) {
	needGit(t)
	dir := filepath.Join(t.TempDir(), "acme")
	f := &fakeDeps{}
	got := Plant(context.Background(), f.deps(), newSeed("prompt", dir))
	if got.Status != protocol.SeedPlanted || got.DefaultBranch != "main" || got.LocalPath != dir || got.RemoteURL != "" {
		t.Fatalf("report = %+v", got)
	}
	if len(f.reports) != 1 || f.reports[0].Status != protocol.SeedPlanting {
		t.Fatalf("planting must be reported first: %+v", f.reports)
	}
	for _, name := range []string{"README.md", "docs/PRODUCT.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Error(err)
		}
	}
	if n := gitOut(t, dir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("commits = %s", n)
	}
	if head := gitOut(t, dir, "rev-parse", "HEAD"); head != got.HeadCommit {
		t.Errorf("head %s != reported %s", head, got.HeadCommit)
	}
	if b := gitOut(t, dir, "branch", "--show-current"); b != "main" {
		t.Errorf("branch = %s", b)
	}
	msg := gitOut(t, dir, "log", "-1", "--format=%B")
	lines := strings.Split(msg, "\n")
	if lines[0] != "Start Acme" || lines[1] != "" || lines[len(lines)-1] != "ProductOS: MOB" {
		t.Errorf("message = %q", msg)
	}
}

func TestExpandsHomeAndAcceptsEmptyDirectory(t *testing.T) {
	needGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Plant(context.Background(), (&fakeDeps{}).deps(), newSeed("prompt", "~/acme"))
	if got.Status != protocol.SeedPlanted || got.LocalPath != filepath.Join(home, "acme") {
		t.Fatalf("report = %+v", got)
	}
}

func TestPathExistsWhenNotEmpty(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "mine.txt")
	_ = os.WriteFile(keep, []byte("x"), 0o644)
	got := Plant(context.Background(), (&fakeDeps{}).deps(), newSeed("prompt", dir))
	if got.Status != protocol.SeedFailed || got.ReasonCode != protocol.SeedPathExists || !strings.Contains(got.Reason, dir) {
		t.Fatalf("report = %+v", got)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the person's file must stay")
	}
}

func TestZipStripsTopLevelDirectoryAndKeepsItsReadme(t *testing.T) {
	needGit(t)
	dir := filepath.Join(t.TempDir(), "zipped")
	f := &fakeDeps{archive: makeZip(t, map[string]string{"app-main/README.md": "# From the zip\n", "app-main/src/main.go": "package main\n"})}
	got := Plant(context.Background(), f.deps(), newSeed("zip", dir))
	if got.Status != protocol.SeedPlanted {
		t.Fatalf("report = %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "# From the zip\n" {
		t.Errorf("README = %q: the archive's own must win", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "main.go")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "PRODUCT.md")); err == nil {
		t.Error("files are only the README fallback for a zip")
	}
}

func TestZipWithoutReadmeGetsTheServersAndKeepsRootFiles(t *testing.T) {
	needGit(t)
	dir := filepath.Join(t.TempDir(), "zipped")
	f := &fakeDeps{archive: makeZip(t, map[string]string{"a.txt": "a", "b/c.txt": "c"})}
	got := Plant(context.Background(), f.deps(), newSeed("zip", dir))
	if got.Status != protocol.SeedPlanted {
		t.Fatalf("report = %+v", got)
	}
	for _, name := range []string{"a.txt", "b/c.txt", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestZipSlipAndOtherBadArchivesAreRefused(t *testing.T) {
	needGit(t)
	many := map[string]string{}
	for i := 0; i < 301; i++ {
		many[fmt.Sprintf("f%d.txt", i)] = "x"
	}
	cases := map[string][]byte{
		"slip":     makeZip(t, map[string]string{"ok.txt": "x", "../evil.txt": "x"}, "ok.txt", "../evil.txt"),
		"absolute": makeZip(t, map[string]string{"/etc/evil": "x"}),
		"too many": makeZip(t, many),
		"too big":  makeZip(t, map[string]string{"big.bin": strings.Repeat("a", 2<<20+1)}),
		"not zip":  []byte("nope"),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "p")
			got := Plant(context.Background(), (&fakeDeps{archive: archive}).deps(), newSeed("zip", dir))
			if got.Status != protocol.SeedFailed || got.ReasonCode != protocol.SeedArchiveInvalid {
				t.Fatalf("report = %+v", got)
			}
			if _, err := os.Stat(filepath.Join(root, "evil.txt")); err == nil {
				t.Error("escaped the directory")
			}
		})
	}
}

func TestZipSymlinkIsRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "link"}
	h.SetMode(os.ModeSymlink | 0o777)
	w, _ := zw.CreateHeader(h)
	_, _ = w.Write([]byte("/etc/passwd"))
	_ = zw.Close()
	needGit(t)
	got := Plant(context.Background(), (&fakeDeps{archive: buf.Bytes()}).deps(), newSeed("zip", filepath.Join(t.TempDir(), "p")))
	if got.ReasonCode != protocol.SeedArchiveInvalid {
		t.Fatalf("report = %+v", got)
	}
}

func TestUnsafeFilePathIsInternal(t *testing.T) {
	needGit(t)
	s := newSeed("prompt", filepath.Join(t.TempDir(), "p"))
	s.Files = []protocol.SeedFile{{Path: "../x", Content: "x"}}
	got := Plant(context.Background(), (&fakeDeps{}).deps(), s)
	if got.Status != protocol.SeedFailed || got.ReasonCode != protocol.SeedInternal {
		t.Fatalf("report = %+v", got)
	}
}

func TestCommitFailedWithoutIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("EMAIL", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "user.useConfigOnly")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_KEY_1", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_1", "")
	got := Plant(context.Background(), (&fakeDeps{}).deps(), newSeed("prompt", filepath.Join(t.TempDir(), "p")))
	if got.Status != protocol.SeedFailed || got.ReasonCode != protocol.SeedCommitFailed || got.Reason == "" {
		t.Fatalf("report = %+v", got)
	}
	if strings.Contains(got.Reason, "\n") {
		t.Errorf("reason must be one line: %q", got.Reason)
	}
}

// fakeGH writes a gh stand-in that records its arguments; with fail it prints two lines to stderr and exits 1.
func fakeGH(t *testing.T, fail bool) (bin, argsFile string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "gh")
	argsFile = filepath.Join(dir, "args")
	body := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n"
	if fail {
		body += "printf '\\nGraphQL: name already exists on this account\\nsecond line\\n' >&2\nexit 1\n"
	} else {
		body += "git remote add origin https://github.com/acme/acme.git\nexit 0\n"
	}
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestGHCreatesTheRemote(t *testing.T) {
	needGit(t)
	bin, argsFile := fakeGH(t, false)
	dir := filepath.Join(t.TempDir(), "acme")
	s := newSeed("prompt", dir)
	s.RepositoryFullName = "acme/acme"
	s.Private = true
	got := Plant(context.Background(), (&fakeDeps{gh: bin}).deps(), s)
	if got.Status != protocol.SeedPlanted || got.RemoteURL != "https://github.com/acme/acme.git" || len(got.Notes) != 0 {
		t.Fatalf("report = %+v", got)
	}
	args, _ := os.ReadFile(argsFile)
	want := "repo create acme/acme --private --source=" + dir + " --remote=origin --push"
	if strings.TrimSpace(string(args)) != want {
		t.Errorf("gh args = %q, want %q", args, want)
	}
	s2 := newSeed("prompt", filepath.Join(t.TempDir(), "pub"))
	s2.RepositoryFullName = "acme/pub"
	Plant(context.Background(), (&fakeDeps{gh: bin}).deps(), s2)
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "--public") {
		t.Errorf("gh args = %q", args)
	}
}

func TestGHFailureStillPlants(t *testing.T) {
	needGit(t)
	bin, _ := fakeGH(t, true)
	s := newSeed("prompt", filepath.Join(t.TempDir(), "acme"))
	s.RepositoryFullName = "acme/acme"
	got := Plant(context.Background(), (&fakeDeps{gh: bin}).deps(), s)
	if got.Status != protocol.SeedPlanted || got.RemoteURL != "" || got.HeadCommit == "" {
		t.Fatalf("report = %+v", got)
	}
	if len(got.Notes) != 1 || got.Notes[0].Code != protocol.NoteRemoteFailed || got.Notes[0].Text != "GraphQL: name already exists on this account" {
		t.Errorf("notes = %+v", got.Notes)
	}
}

func TestNoGHIsANoteNotAFailure(t *testing.T) {
	needGit(t)
	s := newSeed("prompt", filepath.Join(t.TempDir(), "acme"))
	s.RepositoryFullName = "acme/acme"
	got := Plant(context.Background(), (&fakeDeps{}).deps(), s)
	if got.Status != protocol.SeedPlanted || len(got.Notes) != 1 || got.Notes[0].Code != protocol.NoteRemoteFailed {
		t.Fatalf("report = %+v", got)
	}
}
