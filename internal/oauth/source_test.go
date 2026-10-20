package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/productos-technology/productos-agent/internal/secrets"
)

// fakeServer answers discovery and the refresh_token grant: each refresh token in `next` works once and hands out
// the pair it maps to; any other refresh token is refused for good with invalid_grant (as a rotated one would be).
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	next     map[string]secrets.Tokens
	spent    []string
	refreshs atomic.Int32
	// beforeAnswer runs inside the token handler, before the answer, with the refresh token presented.
	beforeAnswer func(refreshToken string)
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{next: map[string]secrets.Tokens{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/oauth/authorize",
			"token_endpoint": base + "/oauth/token", "registration_endpoint": base + "/oauth/register",
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("unexpected grant_type %q", r.Form.Get("grant_type"))
		}
		presented := r.Form.Get("refresh_token")
		fs.refreshs.Add(1)
		if fs.beforeAnswer != nil {
			fs.beforeAnswer(presented)
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.spent = append(fs.spent, presented)
		pair, ok := fs.next[presented]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "the refresh token was revoked, already used or expired"})
			return
		}
		delete(fs.next, presented)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": pair.AccessToken, "token_type": "Bearer", "expires_in": 3600,
			"refresh_token": pair.RefreshToken, "scope": pair.Scope,
		})
	})
	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func isolateStore(t *testing.T) {
	t.Helper()
	t.Setenv("PRODUCTOS_AGENT_CONFIG_DIR", t.TempDir())
	t.Setenv("PRODUCTOS_AGENT_NO_KEYRING", "1")
}

func pair(name string, expiresIn time.Duration) secrets.Tokens {
	return secrets.Tokens{AccessToken: "poa_" + name, RefreshToken: "por_" + name, ExpiresAt: time.Now().Add(expiresIn), Scope: "mcp:read agent"}
}

// loadedSource is a TokenSource that already holds `stored` in memory, like the daemon after its first use.
func loadedSource(t *testing.T, srv *fakeServer, store *secrets.Store, stored secrets.Tokens) *TokenSource {
	t.Helper()
	if err := store.Save(stored); err != nil {
		t.Fatal(err)
	}
	s := NewTokenSource(New(srv.URL), "pcl_test", store)
	if _, err := s.Current(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTokenAdoptsThePairAnotherProcessSaved(t *testing.T) {
	isolateStore(t)
	srv := newFakeServer(t)
	store := secrets.New(srv.URL)
	expired := pair("a", -time.Minute)
	s := loadedSource(t, srv, store, expired)

	// Another process (a CLI command) rotated the refresh token and saved the new pair, still valid for an hour.
	if err := secrets.New(srv.URL).Save(pair("b", time.Hour)); err != nil {
		t.Fatal(err)
	}

	token, err := s.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token != "poa_b" {
		t.Errorf("token = %q, want the pair the other process saved", token)
	}
	if n := srv.refreshs.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0: the spent refresh token must never be presented", n)
	}
}

func TestTokenSpendsTheStoredRefreshTokenNotTheOneInMemory(t *testing.T) {
	isolateStore(t)
	srv := newFakeServer(t)
	store := secrets.New(srv.URL)
	s := loadedSource(t, srv, store, pair("a", -time.Minute))
	// The other process's pair has expired too: the refresh must use its refresh token, not the one in memory.
	if err := secrets.New(srv.URL).Save(pair("b", -time.Minute)); err != nil {
		t.Fatal(err)
	}
	srv.next["por_b"] = pair("c", time.Hour)

	token, err := s.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token != "poa_c" {
		t.Errorf("token = %q, want poa_c", token)
	}
	if len(srv.spent) != 1 || srv.spent[0] != "por_b" {
		t.Errorf("presented %v, want only por_b", srv.spent)
	}
	saved, err := store.Load()
	if err != nil || saved.RefreshToken != "por_c" {
		t.Errorf("store holds %+v (%v), want the new pair", saved, err)
	}
}

func TestTokenRefusedForGoodSignsOut(t *testing.T) {
	isolateStore(t)
	srv := newFakeServer(t)
	store := secrets.New(srv.URL)
	s := loadedSource(t, srv, store, pair("a", -time.Minute))

	_, err := s.Token(t.Context())
	if !errors.Is(err, ErrSignedOut) {
		t.Fatalf("err = %v, want ErrSignedOut", err)
	}
	if _, err := store.Load(); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("store.Load() = %v, want ErrNotFound: the dead credentials are forgotten", err)
	}
	if _, err := s.Token(t.Context()); !errors.Is(err, ErrSignedOut) {
		t.Errorf("second Token() = %v, want ErrSignedOut", err)
	}
	if n := srv.refreshs.Load(); n != 1 {
		t.Errorf("refreshed %d times, want 1: a dead source never asks again", n)
	}
}

func TestTokenRefusedButANewerPairLandedMeanwhile(t *testing.T) {
	isolateStore(t)
	srv := newFakeServer(t)
	store := secrets.New(srv.URL)
	s := loadedSource(t, srv, store, pair("a", -time.Minute))
	// The race: while the server is refusing por_a (another process spent it a moment ago), that process saves its pair.
	srv.beforeAnswer = func(presented string) {
		if presented == "por_a" {
			if err := secrets.New(srv.URL).Save(pair("b", time.Hour)); err != nil {
				t.Error(err)
			}
		}
	}

	token, err := s.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token != "poa_b" {
		t.Errorf("token = %q, want poa_b", token)
	}
	if saved, err := store.Load(); err != nil || saved.RefreshToken != "por_b" {
		t.Errorf("store holds %+v (%v), want por_b kept", saved, err)
	}
}

func TestTokenTransientFailureKeepsTheTokens(t *testing.T) {
	isolateStore(t)
	srv := newFakeServer(t)
	store := secrets.New(srv.URL)
	s := loadedSource(t, srv, store, pair("a", -time.Minute))
	srv.Close() // the network is down

	_, err := s.Token(t.Context())
	if err == nil || errors.Is(err, ErrSignedOut) {
		t.Fatalf("err = %v, want a transient error", err)
	}
	if saved, err := store.Load(); err != nil || saved.RefreshToken != "por_a" {
		t.Errorf("store holds %+v (%v), want por_a kept for a later try", saved, err)
	}
}

func TestConcurrentSourcesRefreshOnce(t *testing.T) {
	isolateStore(t)
	srv := newFakeServer(t)
	expired := pair("a", -time.Minute)
	// Two processes, each with its own source over the same store, both holding the expired pair.
	first := loadedSource(t, srv, secrets.New(srv.URL), expired)
	second := loadedSource(t, srv, secrets.New(srv.URL), expired)
	srv.next["por_a"] = pair("b", time.Hour)

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i, s := range []*TokenSource{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Token(t.Context())
		}()
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Errorf("source %d: %v", i, errs[i])
		}
		if results[i] != "poa_b" {
			t.Errorf("source %d: token = %q, want poa_b", i, results[i])
		}
	}
	if n := srv.refreshs.Load(); n != 1 {
		t.Errorf("refreshed %d times, want 1: the second process adopts what the first saved", n)
	}
}
