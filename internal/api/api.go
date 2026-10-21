// Package api is the device's REST client for /api/v1/agent/* (docs/reference/agent-protocol.md): bearer token, JSON
// envelope errors, one automatic token refresh on 401.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/version"
)

// TokenSource is what the client needs from the OAuth side.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(ctx context.Context, stale string) error
}

// Error is the API envelope {"error": {"code", "message", "details"?}} with its HTTP status.
type Error struct {
	Status  int
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("%s (%d)", e.Code, e.Status)
}

// Is lets errors.Is match on the code: errors.Is(err, &api.Error{Code: "not_found"}).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && (t.Code == "" || t.Code == e.Code) && (t.Status == 0 || t.Status == e.Status)
}

// UnknownRemote is the 422 of POST /devices/{id}/repositories, decoded.
type UnknownRemote struct {
	Remote   *string  `json:"remote"`
	Expected []string `json:"expected"`
}

// Client is bound to one server and one token source.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Tokens  TokenSource
	// Stall is how long a download may receive nothing before it is abandoned (0: a minute).
	Stall time.Duration
}

func New(server string, tokens TokenSource) *Client {
	return &Client{BaseURL: server, HTTP: &http.Client{Timeout: 30 * time.Second}, Tokens: tokens}
}

// --- devices ---

type RegisterDeviceInput struct {
	Name         string         `json:"name"`
	Platform     string         `json:"platform"`
	Arch         string         `json:"arch"`
	AgentVersion string         `json:"agent_version"`
	Tools        protocol.Tools `json:"tools,omitempty"`
	// MaxParallelRuns: how many runs this device takes at once (additive; a server without it keeps one run per
	// checkout).
	MaxParallelRuns int `json:"max_parallel_runs,omitempty"`
}

func (c *Client) RegisterDevice(ctx context.Context, in RegisterDeviceInput) (*protocol.Device, error) {
	var out protocol.Device
	return &out, c.do(ctx, http.MethodPost, "/api/v1/agent/devices", in, &out)
}

type HeartbeatInput struct {
	Tools           protocol.Tools `json:"tools,omitempty"`
	AgentVersion    string         `json:"agent_version,omitempty"`
	Name            string         `json:"name,omitempty"`
	MaxParallelRuns int            `json:"max_parallel_runs,omitempty"`
}

func (c *Client) Heartbeat(ctx context.Context, deviceID string, in HeartbeatInput) (*protocol.Device, error) {
	var out protocol.Device
	return &out, c.do(ctx, http.MethodPatch, "/api/v1/agent/devices/"+url.PathEscape(deviceID), in, &out)
}

func (c *Client) RevokeDevice(ctx context.Context, deviceID string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/agent/devices/"+url.PathEscape(deviceID), nil, nil)
}

// --- repositories ---

type LinkRepositoryInput struct {
	LocalPath     string `json:"local_path"`
	RemoteURL     string `json:"remote_url"`
	DefaultBranch string `json:"default_branch"`
}

func (c *Client) LinkRepository(ctx context.Context, deviceID string, in LinkRepositoryInput) (*protocol.RepositoryLink, error) {
	var out protocol.RepositoryLink
	return &out, c.do(ctx, http.MethodPost, "/api/v1/agent/devices/"+url.PathEscape(deviceID)+"/repositories", in, &out)
}

func (c *Client) UnlinkRepository(ctx context.Context, deviceID, repositoryID string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/agent/devices/"+url.PathEscape(deviceID)+"/repositories/"+url.PathEscape(repositoryID), nil, nil)
}

// --- runs ---

func (c *Client) ActiveRuns(ctx context.Context, deviceID string) ([]protocol.Run, error) {
	var out struct {
		Items []protocol.Run `json:"items"`
	}
	path := "/api/v1/agent/runs"
	if deviceID != "" {
		path += "?device_id=" + url.QueryEscape(deviceID)
	}
	return out.Items, c.do(ctx, http.MethodGet, path, nil, &out)
}

func (c *Client) Run(ctx context.Context, runID string, after int) (*protocol.RunDetail, error) {
	var out protocol.RunDetail
	path := "/api/v1/agent/runs/" + url.PathEscape(runID)
	if after > 0 {
		path += "?after=" + strconv.Itoa(after)
	}
	return &out, c.do(ctx, http.MethodGet, path, nil, &out)
}

type EventsResult struct {
	LastSeq int                 `json:"last_seq"`
	Events  []protocol.RunEvent `json:"events"`
}

func (c *Client) PostEvents(ctx context.Context, runID string, events []protocol.Event) (*EventsResult, error) {
	var out EventsResult
	body := map[string]any{"events": events}
	return &out, c.do(ctx, http.MethodPost, "/api/v1/agent/runs/"+url.PathEscape(runID)+"/events", body, &out)
}

func (c *Client) PostStatus(ctx context.Context, runID string, report protocol.StatusReport) (*protocol.Run, error) {
	var out protocol.Run
	return &out, c.do(ctx, http.MethodPost, "/api/v1/agent/runs/"+url.PathEscape(runID)+"/status", report, &out)
}

// --- seeds ---

func (c *Client) Seed(ctx context.Context, seedID string) (*protocol.Seed, error) {
	var out protocol.Seed
	return &out, c.do(ctx, http.MethodGet, "/api/v1/agent/seeds/"+url.PathEscape(seedID), nil, &out)
}

// maxArchive bounds the zip download: 300 entries of at most 2 MB each, compressed, with room to spare.
const maxArchive = 64 << 20

// SeedArchive downloads the zip of a seed of kind "zip".
func (c *Client) SeedArchive(ctx context.Context, seedID string) ([]byte, error) {
	var raw []byte
	err := c.doWith(ctx, http.MethodGet, "/api/v1/agent/seeds/"+url.PathEscape(seedID)+"/archive", nil, "application/zip", &raw)
	return raw, err
}

// PostSeedStatus reports a seed's progress; the server answers 200 with the row even when it is finished already.
func (c *Client) PostSeedStatus(ctx context.Context, seedID string, report protocol.SeedStatusReport) error {
	return c.do(ctx, http.MethodPost, "/api/v1/agent/seeds/"+url.PathEscape(seedID)+"/status", report, nil)
}

// --- attached files ---

// agentPrefix: the server names downloads by paths of the agent API; nothing else is fetched with the device token.
const agentPrefix = "/api/v1/agent/"

// downloadStall: a download that receives nothing for this long is abandoned (a stalled connection must not hold a
// run until its own timeout).
const downloadStall = 60 * time.Second

// Attachment streams the body of GET path into dst with the device token: path is what run.start named for an
// attached file (/api/v1/agent/runs/{run_id}/attachments/{id}). At most limit bytes are copied, and a longer body is
// an error, never a cut file. A path off this server or outside the agent API is refused before anything is sent.
// No overall timeout (the caller's context bounds it), but no data for a minute ends it; one token refresh on 401.
func (c *Client) Attachment(ctx context.Context, path string, dst io.Writer, limit int64) (int64, error) {
	target, err := c.agentURL(path)
	if err != nil {
		return 0, err
	}
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return 0, err
	}
	n, status, err := c.download(ctx, target, token, dst, limit)
	if status == http.StatusUnauthorized {
		if rerr := c.Tokens.Invalidate(ctx, token); rerr != nil {
			return 0, rerr
		}
		if token, err = c.Tokens.Token(ctx); err != nil {
			return 0, err
		}
		n, _, err = c.download(ctx, target, token, dst, limit)
	}
	return n, err
}

// agentURL resolves a server-named path against BaseURL: a relative path under /api/v1/agent/ (or the same as an
// absolute URL of this server), already clean (no `..`, no encoded separators that clean to something else).
func (c *Client) agentURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	base := strings.TrimRight(c.BaseURL, "/")
	if rest, ok := strings.CutPrefix(raw, base); ok && base != "" && strings.HasPrefix(rest, "/") {
		raw = rest
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("refused the download path %q: %w", raw, err)
	}
	if u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return "", fmt.Errorf("refused the download path %q: not on this server", raw)
	}
	if p := u.Path; !strings.HasPrefix(p, agentPrefix) || path.Clean(p) != p {
		return "", fmt.Errorf("refused the download path %q: not a path of the agent API", raw)
	}
	target := base + u.EscapedPath()
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target, nil
}

// download is one attempt; the status lets Attachment refresh the token on a 401.
func (c *Client) download(ctx context.Context, target, token string, dst io.Writer, limit int64) (int64, int, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := c.Stall
	if stall <= 0 {
		stall = downloadStall
	}
	stalled := fmt.Errorf("no data from the server for %s", stall)
	timer := time.AfterFunc(stall, func() { cancel(stalled) })
	defer timer.Stop()
	because := func(err error) error {
		if errors.Is(context.Cause(ctx), stalled) {
			return stalled
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "productos-agent/"+version.String())
	res, err := c.downloader().Do(req)
	if err != nil {
		return 0, 0, because(err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return 0, res.StatusCode, envelopeError(res.StatusCode, data)
	}
	if res.StatusCode != http.StatusOK {
		return 0, res.StatusCode, fmt.Errorf("unexpected answer %d %s", res.StatusCode, http.StatusText(res.StatusCode))
	}
	if res.ContentLength > limit {
		return 0, res.StatusCode, fmt.Errorf("the server sends %d bytes, %d expected", res.ContentLength, limit)
	}
	n, err := io.Copy(dst, &stallReader{r: io.LimitReader(res.Body, limit+1), timer: timer, every: stall})
	if err != nil {
		return n, res.StatusCode, because(err)
	}
	if n > limit {
		return n, res.StatusCode, fmt.Errorf("the server sent more than the %d bytes expected", limit)
	}
	return n, res.StatusCode, nil
}

// downloader is the client without the 30 s overall timeout (a large file takes longer); it follows at most five
// redirects and never from https to http. Go drops the Authorization header on a redirect to another host.
func (c *Client) downloader() *http.Client {
	var transport http.RoundTripper
	if c.HTTP != nil {
		transport = c.HTTP.Transport
	}
	return &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refused a redirect from https to http")
		}
		return nil
	}}
}

// stallReader pushes the stall deadline back on every read that brings data.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	every time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.every)
	}
	return n, err
}

// --- plumbing ---

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doWith(ctx, method, path, in, "application/json", out)
}

// doWith is do with an explicit Accept; an out of type *[]byte receives the body as it is instead of decoded JSON.
func (c *Client) doWith(ctx context.Context, method, path string, in any, accept string, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	status, err := c.once(ctx, method, path, body, accept, token, out)
	if status == http.StatusUnauthorized {
		if rerr := c.Tokens.Invalidate(ctx, token); rerr != nil {
			return rerr
		}
		token, err = c.Tokens.Token(ctx)
		if err != nil {
			return err
		}
		_, err = c.once(ctx, method, path, body, accept, token, out)
	}
	return err
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, accept, token string, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "productos-agent/"+version.String())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxArchive))
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 400 {
		return res.StatusCode, envelopeError(res.StatusCode, data)
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return res.StatusCode, nil
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return res.StatusCode, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return res.StatusCode, fmt.Errorf("response is not the expected JSON: %w", err)
	}
	return res.StatusCode, nil
}

// envelopeError decodes the API's error envelope; a body that is not one keeps the HTTP status text as its code.
func envelopeError(status int, data []byte) *Error {
	apiErr := &Error{Status: status}
	var envelope struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
		apiErr.Code, apiErr.Message, apiErr.Details = envelope.Error.Code, envelope.Error.Message, envelope.Error.Details
	}
	if apiErr.Code == "" {
		apiErr.Code = http.StatusText(status)
	}
	return apiErr
}

// IsCode reports whether err is an API error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
