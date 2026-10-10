// Package oauth is the agent's OAuth 2.1 client against ProductOS (docs/reference/mcp.md): RFC 8414 discovery,
// RFC 7591 dynamic registration, the authorization code flow with PKCE S256 and a loopback redirect, token refresh
// with rotation, and revocation. Scope `agent`, resource `${server}/mcp`.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/productos-technology/productos-agent/internal/secrets"
)

const (
	Scope       = "agent"
	ClientName  = "ProductOS Agent"
	httpTimeout = 20 * time.Second
)

// Metadata is the RFC 8414 document.
type Metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// Client talks to one server. HTTP is the plain standard library.
type Client struct {
	Server string // https://productos.technology, no trailing slash
	HTTP   *http.Client
	meta   *Metadata
}

func New(server string) *Client {
	return &Client{Server: NormalizeServer(server), HTTP: &http.Client{Timeout: httpTimeout}}
}

// NormalizeServer trims whitespace and trailing slashes and adds https:// when the scheme is missing.
func NormalizeServer(server string) string {
	s := strings.TrimSpace(server)
	s = strings.TrimRight(s, "/")
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

// Resource is the RFC 8707 resource indicator ProductOS expects.
func (c *Client) Resource() string { return c.Server + "/mcp" }

// Discover fetches /.well-known/oauth-authorization-server (cached on the client).
func (c *Client) Discover(ctx context.Context) (*Metadata, error) {
	if c.meta != nil {
		return c.meta, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Server+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", c.Server, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s is not a ProductOS server (discovery answered %d)", c.Server, res.StatusCode)
	}
	var meta Metadata
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("discovery document is not JSON: %w", err)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" || meta.RegistrationEndpoint == "" {
		return nil, errors.New("discovery document lacks the OAuth endpoints")
	}
	c.meta = &meta
	return c.meta, nil
}

// Register performs RFC 7591 dynamic registration as a public client with loopback redirects; returns client_id.
func (c *Client) Register(ctx context.Context) (string, error) {
	meta, err := c.Discover(ctx)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"client_name":                ClientName,
		"redirect_uris":              []string{"http://127.0.0.1/callback", "http://localhost/callback"},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      Scope,
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := c.postJSON(ctx, meta.RegistrationEndpoint, body, &out, http.StatusCreated, http.StatusOK); err != nil {
		return "", fmt.Errorf("client registration failed: %w", err)
	}
	if out.ClientID == "" {
		return "", errors.New("client registration returned no client_id")
	}
	return out.ClientID, nil
}

// AuthorizeURL builds the browser URL of the authorization request.
func (c *Client) AuthorizeURL(meta *Metadata, clientID, redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", Scope)
	q.Set("resource", c.Resource())
	sep := "?"
	if strings.Contains(meta.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return meta.AuthorizationEndpoint + sep + q.Encode()
}

// TokenError is an RFC 6749 section 5.2 error.
type TokenError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
	Status      int    `json:"-"`
}

func (e *TokenError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// Permanent reports whether a refresh with this error can never succeed (the person must sign in again).
func (e *TokenError) Permanent() bool {
	switch e.Code {
	case "invalid_grant", "invalid_client", "invalid_scope", "invalid_target", "unauthorized_client":
		return true
	}
	return false
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// Exchange turns an authorization code into tokens.
func (c *Client) Exchange(ctx context.Context, clientID, code, verifier, redirectURI string) (secrets.Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("resource", c.Resource())
	return c.token(ctx, form)
}

// Refresh rotates the refresh token and returns the new pair.
func (c *Client) Refresh(ctx context.Context, clientID, refreshToken string) (secrets.Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	return c.token(ctx, form)
}

// Revoke tells the server to forget a token (RFC 7009); a refresh token takes its family with it.
func (c *Client) Revoke(ctx context.Context, clientID, token, hint string) error {
	meta, err := c.Discover(ctx)
	if err != nil {
		return err
	}
	if meta.RevocationEndpoint == "" {
		return nil
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", clientID)
	if hint != "" {
		form.Set("token_type_hint", hint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 400 {
		return fmt.Errorf("revocation answered %d", res.StatusCode)
	}
	return nil
}

func (c *Client) token(ctx context.Context, form url.Values) (secrets.Tokens, error) {
	meta, err := c.Discover(ctx)
	if err != nil {
		return secrets.Tokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return secrets.Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return secrets.Tokens{}, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return secrets.Tokens{}, err
	}
	if res.StatusCode != http.StatusOK {
		te := &TokenError{Status: res.StatusCode}
		if json.Unmarshal(data, te) != nil || te.Code == "" {
			te.Code = "server_error"
			te.Description = fmt.Sprintf("token endpoint answered %d", res.StatusCode)
		}
		return secrets.Tokens{}, te
	}
	var tr tokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return secrets.Tokens{}, fmt.Errorf("token response is not JSON: %w", err)
	}
	if tr.AccessToken == "" {
		return secrets.Tokens{}, errors.New("token response has no access_token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	return secrets.Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scope:        tr.Scope,
	}, nil
}

func (c *Client) postJSON(ctx context.Context, endpoint string, body any, out any, okStatuses ...int) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	ok := false
	for _, s := range okStatuses {
		ok = ok || res.StatusCode == s
	}
	if !ok {
		te := &TokenError{Status: res.StatusCode}
		if json.Unmarshal(raw, te) == nil && te.Code != "" {
			return te
		}
		return fmt.Errorf("answered %d", res.StatusCode)
	}
	return json.Unmarshal(raw, out)
}
