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
	"strconv"
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
}

func (c *Client) RegisterDevice(ctx context.Context, in RegisterDeviceInput) (*protocol.Device, error) {
	var out protocol.Device
	return &out, c.do(ctx, http.MethodPost, "/api/v1/agent/devices", in, &out)
}

type HeartbeatInput struct {
	Tools        protocol.Tools `json:"tools,omitempty"`
	AgentVersion string         `json:"agent_version,omitempty"`
	Name         string         `json:"name,omitempty"`
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
		apiErr := &Error{Status: res.StatusCode}
		var envelope struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
			apiErr.Code, apiErr.Message, apiErr.Details = envelope.Error.Code, envelope.Error.Message, envelope.Error.Details
		}
		if apiErr.Code == "" {
			apiErr.Code = http.StatusText(res.StatusCode)
		}
		return res.StatusCode, apiErr
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

// IsCode reports whether err is an API error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
