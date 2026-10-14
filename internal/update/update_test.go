package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"v0.2.0", "0.2.0", 0},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.2.0-rc.1", "0.2.0", -1},
		{"0.2.0-rc.2", "0.2.0-rc.10", -1},
		{"0.2.0-alpha", "0.2.0-1", 1},
		{"0.0.0-dev", "0.1.0", -1},
		{"garbage", "0.0.1", -1},
		{"0.2.0+build.5", "v0.2.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if !Behind("0.1.0", "v0.2.0") || Behind("0.2.0", "0.2.0") || Behind("0.2.0", "") || Behind("0.3.0", "0.2.0") {
		t.Error("Behind")
	}
	for v, dev := range map[string]bool{"0.0.0-dev": true, "1.2.3-dev.4": true, "unknown": true, "0.2.0": false, "v1.0.0-rc.1": false} {
		if IsDev(v) != dev {
			t.Errorf("IsDev(%q) = %v", v, !dev)
		}
	}
}

func TestDetectMethod(t *testing.T) {
	cases := map[string]Method{
		"/opt/homebrew/Cellar/productos-agent/0.1.0/bin/productos-agent":                                                                 MethodHomebrew,
		"/opt/homebrew/bin/productos-agent":                                                                                              MethodHomebrew,
		"/usr/local/Cellar/productos-agent/0.1.0/bin/productos-agent":                                                                    MethodHomebrew,
		"/home/linuxbrew/.linuxbrew/Cellar/productos-agent/0.1.0/bin/productos-agent":                                                    MethodHomebrew,
		`C:\Users\pat\AppData\Local\Microsoft\WinGet\Packages\ProductOS.Agent_Microsoft.Winget.Source_8wekyb3d8bbwe\productos-agent.exe`: MethodWinget,
		`C:\Users\pat\AppData\Local\Microsoft\WinGet\Links\productos-agent.exe`:                                                          MethodWinget,
		"/usr/local/bin/productos-agent":                                                                                                 MethodSelf,
		"/home/pat/.local/bin/productos-agent":                                                                                           MethodSelf,
		`C:\Tools\productos-agent.exe`:                                                                                                   MethodSelf,
	}
	for p, want := range cases {
		if got := DetectMethod(p); got != want {
			t.Errorf("DetectMethod(%q) = %s, want %s", p, got, want)
		}
	}
	// A symlink in /usr/local/bin that resolves into the Cellar is Homebrew's.
	if DetectMethod("/usr/local/bin/productos-agent", "/usr/local/Cellar/productos-agent/0.1.0/bin/productos-agent") != MethodHomebrew {
		t.Error("the resolved path decides")
	}
	if MethodHomebrew.Command() != "brew upgrade productos-agent" || MethodWinget.Command() != "winget upgrade ProductOS.Agent" || MethodSelf.Command() != "" {
		t.Error("Command")
	}
}

func TestArchiveName(t *testing.T) {
	if got := ArchiveName("v0.2.0", "linux", "amd64"); got != "productos-agent_0.2.0_linux_amd64.tar.gz" {
		t.Error(got)
	}
	if got := ArchiveName("0.2.0", "windows", "arm64"); got != "productos-agent_0.2.0_windows_arm64.zip" {
		t.Error(got)
	}
}

// --- a fake release ---

func tarGz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

const fakeBinary = "#!/bin/sh\necho \"productos-agent 0.2.0 (abc123, 2026-10-14)\"\n"

// release serves /v0.2.0/<archive> and /v0.2.0/checksums.txt over TLS; sum overrides the archive's checksum.
func release(t *testing.T, archive string, data []byte, sum string) *httptest.Server {
	if sum == "" {
		sum = sha(data)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/download/v0.2.0/"+archive, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/download/v0.2.0/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  productos-agent_0.2.0_darwin_arm64.tar.gz\n%s  %s\n", strings.Repeat("0", 64), sum, archive)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestStageVerifiesExtractsAndSmokes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	archive := ArchiveName("0.2.0", "linux", "amd64")
	data := tarGz(t, map[string]string{"README.md": "# readme", "LICENSE": "MIT", "productos-agent": fakeBinary})
	srv := release(t, archive, data, "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "productos-agent")
	_ = os.WriteFile(exe, []byte("old"), 0o755)

	u := &Updater{Base: srv.URL + "/download", Exe: exe, GOOS: "linux", GOARCH: "amd64", HTTP: srv.Client()}
	staged, err := u.Stage(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(staged) != dir {
		t.Errorf("staged %s outside the binary's directory", staged)
	}
	if got, _ := os.ReadFile(staged); string(got) != fakeBinary {
		t.Errorf("staged = %q", got)
	}
}

func TestStageRefusesAChecksumMismatch(t *testing.T) {
	archive := ArchiveName("0.2.0", "linux", "amd64")
	data := tarGz(t, map[string]string{"productos-agent": fakeBinary})
	srv := release(t, archive, data, strings.Repeat("ab", 32))
	dir := t.TempDir()
	u := &Updater{Base: srv.URL + "/download", Exe: filepath.Join(dir, "productos-agent"), GOOS: "linux", GOARCH: "amd64", HTTP: srv.Client(),
		Smoke: func(context.Context, string, string) error {
			t.Error("a mismatched archive must not be smoke-tested")
			return nil
		}}
	_, err := u.Stage(context.Background(), "0.2.0")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".productos-agent-*")); len(left) != 0 {
		t.Errorf("nothing may be staged: %v", left)
	}
}

func TestStageRefusesPlainHTTPAndAFailedSmokeTest(t *testing.T) {
	u := &Updater{Base: "http://example.com/download", Exe: filepath.Join(t.TempDir(), "x"), GOOS: "linux", GOARCH: "amd64"}
	if _, err := u.Stage(context.Background(), "0.2.0"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v", err)
	}
	archive := ArchiveName("0.2.0", "linux", "amd64")
	srv := release(t, archive, tarGz(t, map[string]string{"productos-agent": "x"}), "")
	dir := t.TempDir()
	u = &Updater{Base: srv.URL + "/download", Exe: filepath.Join(dir, "productos-agent"), GOOS: "linux", GOARCH: "amd64", HTTP: srv.Client(),
		Smoke: func(context.Context, string, string) error { return errors.New("does not start") }}
	if _, err := u.Stage(context.Background(), "0.2.0"); err == nil {
		t.Error("a failed smoke test must fail the stage")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".productos-agent-*")); len(left) != 0 {
		t.Errorf("the staged file must be removed: %v", left)
	}
}

func TestExtract(t *testing.T) {
	bin, err := extract(tarGz(t, map[string]string{"README.md": "r", "productos-agent": "BIN"}), "productos-agent_0.2.0_linux_amd64.tar.gz", "linux")
	if err != nil || string(bin) != "BIN" {
		t.Errorf("tar.gz: %q %v", bin, err)
	}
	bin, err = extract(zipped(t, map[string]string{"LICENSE": "l", "productos-agent.exe": "EXE"}), "productos-agent_0.2.0_windows_amd64.zip", "windows")
	if err != nil || string(bin) != "EXE" {
		t.Errorf("zip: %q %v", bin, err)
	}
	if _, err := extract(tarGz(t, map[string]string{"README.md": "r"}), "a.tar.gz", "linux"); err == nil {
		t.Error("an archive without the binary must fail")
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte(strings.Repeat("a", 64) + "  productos-agent_0.2.0_linux_amd64.tar.gz\n" + strings.Repeat("b", 64) + " *productos-agent_0.2.0_windows_amd64.zip\n")
	if got, err := checksumFor(sums, "productos-agent_0.2.0_windows_amd64.zip"); err != nil || got != strings.Repeat("b", 64) {
		t.Errorf("got %q %v", got, err)
	}
	if _, err := checksumFor(sums, "productos-agent_0.2.0_linux_arm64.tar.gz"); err == nil {
		t.Error("a missing line must fail")
	}
}

func TestReplaceInATempDir(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "productos-agent")
	staged := filepath.Join(dir, ".productos-agent-0.2.0.new")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	_ = os.WriteFile(staged, []byte("new"), 0o644)
	before, _ := os.Stat(exe)
	// A process holding the old binary open keeps reading the old bytes (Unix: new inode on the path).
	held, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := Replace(staged, exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Errorf("binary = %q", got)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staged file must be gone")
	}
	if runtime.GOOS != "windows" {
		after, _ := os.Stat(exe)
		if os.SameFile(before, after) {
			t.Error("the path must point to a new inode")
		}
		if after.Mode().Perm()&0o111 == 0 {
			t.Errorf("mode = %v", after.Mode())
		}
		old := make([]byte, 3)
		if _, err := held.ReadAt(old, 0); err != nil || string(old) != "old" {
			t.Errorf("the running binary's file changed: %q %v", old, err)
		}
	}
	CleanOld(exe)
}

func TestLatestReadsTheRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s", r.Method)
		}
		http.Redirect(w, r, "/productos-technology/productos-agent/releases/tag/v0.3.1", http.StatusFound)
	}))
	defer srv.Close()
	got, err := Latest(context.Background(), srv.Client(), srv.URL+"/releases/latest")
	if err != nil || got != "v0.3.1" {
		t.Errorf("Latest = %q, %v", got, err)
	}
}

func TestCleanOldRemovesLeftovers(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "productos-agent")
	for _, f := range []string{exe, exe + ".old", filepath.Join(dir, ".productos-agent-0.2.0.new"), filepath.Join(dir, "keep.txt")} {
		_ = os.WriteFile(f, []byte("x"), 0o644)
	}
	CleanOld(exe)
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "keep.txt,productos-agent" {
		t.Errorf("left = %v", names)
	}
}
