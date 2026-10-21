package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rotatingTokens hands out "poa_old" until it is invalidated, then "poa_new".
type rotatingTokens struct {
	mu          sync.Mutex
	invalidated bool
}

func (r *rotatingTokens) Token(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.invalidated {
		return "poa_new", nil
	}
	return "poa_old", nil
}

func (r *rotatingTokens) Invalidate(context.Context, string) error {
	r.mu.Lock()
	r.invalidated = true
	r.mu.Unlock()
	return nil
}

const attachmentPath = "/api/v1/agent/runs/run-1/attachments/att-1"

func TestAttachmentStreamsWithTheDeviceToken(t *testing.T) {
	body := bytes.Repeat([]byte("spreadsheet,"), 10_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != attachmentPath || r.Header.Get("Authorization") != "Bearer poa_old" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="figures.csv"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c := New(srv.URL, &rotatingTokens{})
	var dst bytes.Buffer
	n, err := c.Attachment(context.Background(), attachmentPath, &dst, int64(len(body)))
	if err != nil || n != int64(len(body)) || !bytes.Equal(dst.Bytes(), body) {
		t.Fatalf("n = %d, err = %v, same body = %v", n, err, bytes.Equal(dst.Bytes(), body))
	}
	// The same file named by an absolute URL of this server is fine too.
	dst.Reset()
	if _, err := c.Attachment(context.Background(), srv.URL+attachmentPath, &dst, int64(len(body))); err != nil || dst.Len() != len(body) {
		t.Fatalf("absolute URL of the server: %v", err)
	}
}

func TestAttachmentRefreshesTheTokenOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer poa_new" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"expired"}}`))
			return
		}
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()
	tokens := &rotatingTokens{}
	var dst bytes.Buffer
	n, err := New(srv.URL, tokens).Attachment(context.Background(), attachmentPath, &dst, 5)
	if err != nil || n != 5 || dst.String() != "hello" || calls.Load() != 2 || !tokens.invalidated {
		t.Fatalf("n = %d, err = %v, body = %q, calls = %d", n, err, dst.String(), calls.Load())
	}
}

func TestAttachmentRefusesPathsOffTheAgentAPI(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := New(srv.URL, &rotatingTokens{})
	for _, path := range []string{
		"https://evil.example" + attachmentPath,
		"//evil.example" + attachmentPath,
		"/api/v1/w/acme/changes/MOB-C4/attachments/att-1",
		"/api/v1/agent/runs/run-1/../../../w/acme/changes",
		"/api/v1/agent/runs/run-1/attachments/%2e%2e/%2e%2e/devices",
		"/api/v1/agent/runs/run-1//attachments/att-1",
		"api/v1/agent/runs/run-1/attachments/att-1",
		"javascript:alert(1)",
		"",
	} {
		var dst bytes.Buffer
		if _, err := c.Attachment(context.Background(), path, &dst, 10); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%q: err = %v", path, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("a refused path never reaches any server: %d calls", calls.Load())
	}
}

func TestAttachmentNeverWritesMoreThanTheLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("chunked") == "1" {
			// No Content-Length: the copy itself must stop.
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte("0123456789A"))
	}))
	defer srv.Close()
	c := New(srv.URL, &rotatingTokens{})
	for _, path := range []string{attachmentPath, attachmentPath + "?chunked=1"} {
		var dst bytes.Buffer
		_, err := c.Attachment(context.Background(), path, &dst, 10)
		if err == nil || dst.Len() > 11 {
			t.Errorf("%s: err = %v, wrote %d bytes", path, err, dst.Len())
		}
	}
}

func TestAttachmentErrorsAreTheEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such file on this run"}}`))
	}))
	defer srv.Close()
	var dst bytes.Buffer
	_, err := New(srv.URL, &rotatingTokens{}).Attachment(context.Background(), attachmentPath, &dst, 10)
	if !IsCode(err, "not_found") || dst.Len() != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestAttachmentGivesUpOnAStalledConnection(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("first bytes"))
		w.(http.Flusher).Flush()
		select { // then nothing
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	c := New(srv.URL, &rotatingTokens{})
	c.Stall = 200 * time.Millisecond
	started := time.Now()
	var dst bytes.Buffer
	_, err := c.Attachment(context.Background(), attachmentPath, &dst, 100)
	if err == nil || !strings.Contains(err.Error(), "no data from the server") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Errorf("the stall guard took %s", time.Since(started))
	}
}
