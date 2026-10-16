// Package update replaces the running agent with a release (docs/reference/agent-protocol.md, Agent versions and
// self-update): it compares versions, tells how the binary was installed (only its own installs update themselves;
// Homebrew and winget own theirs), downloads checksums.txt, its Ed25519 signature and the archive over HTTPS, verifies
// the signature against the keys compiled into sign.go and the SHA-256 (what install.sh does), extracts the binary,
// smoke-tests it, swaps it in atomically and re-executes. Standard library only.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBase is where releases live when the server sent no download_base.
	DefaultBase = "https://github.com/productos-technology/productos-agent/releases/download"
	// LatestURL redirects to the newest release's tag page: read from its Location header, no API call (the API allows
	// 60 anonymous requests per hour per IP).
	LatestURL = "https://github.com/productos-technology/productos-agent/releases/latest"
	// Binary is the executable's name inside the archives (with .exe on Windows).
	Binary = "productos-agent"
	// ReplacesEnv carries the PID of the process a Windows re-exec replaces: the new process skips the "another agent
	// is already running" check for it.
	ReplacesEnv = "PRODUCTOS_AGENT_REPLACES"

	maxArchive   = 200 << 20
	maxChecksums = 1 << 20
	maxSignature = 4 << 10
)

// --- versions ---

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

type semver struct {
	nums [3]int
	pre  string
	ok   bool
}

func parse(v string) semver {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return semver{}
	}
	s := semver{pre: m[4], ok: true}
	for i := 0; i < 3; i++ {
		s.nums[i], _ = strconv.Atoi(m[i+1])
	}
	return s
}

// Valid reports whether v is a semantic version (a leading v is fine).
func Valid(v string) bool { return parse(v).ok }

// Number strips the leading v: "v0.2.0" -> "0.2.0" (archive names use the number).
func Number(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Compare orders two versions like semver: -1 when a < b, 0 when equal, 1 when a > b. A pre-release sorts before its
// release (0.2.0-rc.1 < 0.2.0); something that is not a version sorts before every version.
func Compare(a, b string) int {
	x, y := parse(a), parse(b)
	switch {
	case !x.ok && !y.ok:
		return 0
	case !x.ok:
		return -1
	case !y.ok:
		return 1
	}
	for i := 0; i < 3; i++ {
		if x.nums[i] != y.nums[i] {
			return sign(x.nums[i] - y.nums[i])
		}
	}
	return comparePre(x.pre, y.pre)
}

func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				return sign(an - bn)
			}
		case aerr == nil:
			return -1 // numeric identifiers sort before alphanumeric ones
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(as) - len(bs))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// IsDev reports a development build (0.0.0-dev, anything with a -dev pre-release, or not a version at all): it never
// updates itself, so a developer's binary is not replaced behind their back.
func IsDev(v string) bool {
	s := parse(v)
	return !s.ok || s.nums == [3]int{} || strings.Contains(s.pre, "dev")
}

// Behind reports whether current should move to latest.
func Behind(current, latest string) bool {
	return Valid(latest) && Compare(current, latest) < 0
}

// --- install method ---

// Method is how the binary was installed: only MethodSelf (install.sh, a downloaded archive) updates itself.
type Method string

const (
	MethodSelf     Method = "self"
	MethodHomebrew Method = "homebrew"
	MethodWinget   Method = "winget"
)

// DetectMethod reads the binary's paths (as invoked and resolved): Homebrew keeps its kegs under a Cellar
// (/opt/homebrew, /usr/local/Cellar, Linuxbrew), winget its portable packages under WinGet\Packages (with an alias in
// WinGet\Links).
func DetectMethod(paths ...string) Method {
	for _, p := range paths {
		p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
		switch {
		case p == "":
		case strings.Contains(p, "/cellar/"), strings.HasPrefix(p, "/opt/homebrew/"), strings.Contains(p, "/.linuxbrew/"):
			return MethodHomebrew
		case strings.Contains(p, "/winget/packages/"), strings.Contains(p, "/winget/links/"):
			return MethodWinget
		}
	}
	return MethodSelf
}

// Command is what a person runs to update a package-managed install (empty for MethodSelf).
func (m Method) Command() string {
	switch m {
	case MethodHomebrew:
		return "brew upgrade productos-agent"
	case MethodWinget:
		return "winget upgrade ProductOS.Agent"
	}
	return ""
}

// Executable returns the running binary as invoked and resolved (symlinks followed): the resolved file is the one to
// replace, and both paths tell the install method.
func Executable() (invoked, resolved string, err error) {
	invoked, err = os.Executable()
	if err != nil {
		return "", "", err
	}
	resolved, err = filepath.EvalSymlinks(invoked)
	if err != nil {
		return invoked, invoked, nil
	}
	return invoked, resolved, nil
}

// --- download, verify, extract ---

// ArchiveName is goreleaser's naming (install.sh depends on it too): productos-agent_0.2.0_linux_amd64.tar.gz.
func ArchiveName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", Binary, Number(version), goos, goarch, ext)
}

// Updater stages a release next to the binary it will replace.
type Updater struct {
	// Base is the server's download_base (DefaultBase when empty); it must be HTTPS.
	Base string
	// Exe is the binary to replace (resolved).
	Exe          string
	GOOS, GOARCH string
	HTTP         *http.Client
	// Smoke checks the staged binary; the default runs `<staged> version` and looks for the version.
	Smoke func(ctx context.Context, path, version string) error
	// Keys verify checksums.txt.sig; nil means the keys compiled into sign.go (tests set their own).
	Keys []ed25519.PublicKey
}

// New is an Updater for this process.
func New(base, exe string) *Updater {
	return &Updater{Base: base, Exe: exe, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
}

// Stage downloads version's checksums.txt and its signature, verifies the signature with the release keys, downloads
// the archive, verifies its SHA-256, extracts the binary into a new file in the binary's own directory (so the swap is
// a rename on one filesystem) and smoke-tests it. It returns the staged path; Replace swaps it in. The download base
// only has to be HTTPS: whoever serves the files, only an archive the release key signed for is installed.
func (u *Updater) Stage(ctx context.Context, version string) (string, error) {
	if !Valid(version) {
		return "", fmt.Errorf("%q is not a version", version)
	}
	keys, err := u.keys()
	if err != nil {
		return "", err
	}
	num := Number(version)
	base := strings.TrimRight(firstNonEmpty(u.Base, DefaultBase), "/")
	if parsed, err := url.Parse(base); err != nil || parsed.Scheme != "https" {
		return "", fmt.Errorf("the download base %q is not an https URL", base)
	}
	archive := ArchiveName(num, u.GOOS, u.GOARCH)
	dir := base + "/v" + num
	sums, err := u.get(ctx, dir+"/checksums.txt", maxChecksums)
	if err != nil {
		return "", err
	}
	sig, err := u.get(ctx, dir+"/"+SignatureName, maxSignature)
	if err != nil {
		return "", fmt.Errorf("the release is not signed: %w", err)
	}
	if err := VerifySignature(sums, sig, keys); err != nil {
		return "", err
	}
	want, err := checksumFor(sums, archive)
	if err != nil {
		return "", err
	}
	data, err := u.get(ctx, dir+"/"+archive, maxArchive)
	if err != nil {
		return "", err
	}
	if err := verify(data, want); err != nil {
		return "", fmt.Errorf("%s: %w", archive, err)
	}
	bin, err := extract(data, archive, u.GOOS)
	if err != nil {
		return "", err
	}
	staged := filepath.Join(filepath.Dir(u.Exe), "."+Binary+"-"+num+".new")
	if u.GOOS == "windows" {
		staged += ".exe"
	}
	_ = os.Remove(staged)
	if err := os.WriteFile(staged, bin, 0o755); err != nil {
		return "", fmt.Errorf("cannot write next to %s (reinstall to update): %w", u.Exe, err)
	}
	smoke := u.Smoke
	if smoke == nil {
		smoke = runVersion
	}
	if err := smoke(ctx, staged, num); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	return staged, nil
}

func (u *Updater) keys() ([]ed25519.PublicKey, error) {
	if u.Keys != nil {
		return u.Keys, nil
	}
	return ParsePublicKeys(PublicKeys)
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute, CheckRedirect: httpsOnly}
}

// httpsOnly follows redirects (GitHub sends downloads to its object store) but never to plain HTTP.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
	}
	return nil
}

func (u *Updater) get(ctx context.Context, target string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", Binary)
	res, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", target, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", target, limit)
	}
	return data, nil
}

// checksumFor finds the archive's line in checksums.txt ("<sha256>  <name>").
func checksumFor(sums []byte, archive string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archive {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 64 {
				return "", fmt.Errorf("checksums.txt: malformed line for %s", archive)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no line for %s", archive)
}

func verify(data []byte, want string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch (got %s, want %s)", got, want)
	}
	return nil
}

// extract returns the binary's bytes from a goreleaser archive (tar.gz, or zip on Windows).
func extract(data []byte, archive, goos string) ([]byte, error) {
	name := Binary
	if goos == "windows" {
		name += ".exe"
	}
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) != name || f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return readLimited(rc)
		}
		return nil, fmt.Errorf("%s has no %s", archive, name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no %s", archive, name)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == name {
			return readLimited(tr)
		}
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxArchive+1))
	if err == nil && len(data) > maxArchive {
		err = errors.New("the binary is too large")
	}
	return data, err
}

// runVersion is the smoke test: the new binary starts on this machine and says it is the version we asked for.
func runVersion(ctx context.Context, staged, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, staged, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the new binary does not start: %v %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), " "+version+" ") && !strings.Contains(string(out), " "+version+"\n") {
		return fmt.Errorf("the new binary says %q, not %s", strings.TrimSpace(string(out)), version)
	}
	return nil
}

// --- the newest release, without the server ---

// Latest reads the newest release's version from the redirect of latestURL (LatestURL in production): the manual
// `update` command's fallback when the server cannot say.
func Latest(ctx context.Context, client *http.Client, latestURL string) (string, error) {
	c := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, latestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", Binary)
	res, err := c.Do(req)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	loc := res.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("no release found at %s (HTTP %d)", latestURL, res.StatusCode)
	}
	tag, err := url.PathUnescape(loc[i+len("/tag/"):])
	if err != nil || !Valid(tag) {
		return "", fmt.Errorf("the latest release tag %q is not a version", loc[i+len("/tag/"):])
	}
	return tag, nil
}

// CleanOld removes what an earlier update left next to the binary: the Windows .old copy, staged files that never
// went in.
func CleanOld(exe string) {
	_ = os.Remove(exe + ".old")
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "."+Binary+"-*.new*"))
	for _, f := range leftovers {
		_ = os.Remove(f)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
