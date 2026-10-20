package oauth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/productos-technology/productos-agent/internal/secrets"
)

// ErrSignedOut: there are no credentials, or the refresh token was refused for good. The person must `login` again.
var ErrSignedOut = errors.New("not signed in: run `productos-agent login`")

// TokenSource hands out a live access token, refreshing it (with rotation) when it is about to expire or when the
// server refused it. Safe for concurrent use; one refresh at a time.
//
// The store is shared by every productos-agent process on this machine: the daemon and any CLI command run beside it
// (`link`, `status`, `doctor`, `update`) each build their own TokenSource over the same keychain entry or file. A
// refresh spends the refresh token (the server rotates it and, when the spent one is presented again, revokes the
// whole family), so before spending what it holds in memory a TokenSource takes the store's lock and reads the store
// again: a pair another process saved meanwhile is adopted instead.
type TokenSource struct {
	client   *Client
	clientID string
	store    *secrets.Store

	mu     sync.Mutex
	tokens secrets.Tokens
	loaded bool
	dead   error
}

func NewTokenSource(client *Client, clientID string, store *secrets.Store) *TokenSource {
	return &TokenSource{client: client, clientID: clientID, store: store}
}

const refreshMargin = 60 * time.Second

// Token returns a valid access token, refreshing first when it expires within a minute.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead != nil {
		return "", s.dead
	}
	if err := s.loadLocked(); err != nil {
		return "", err
	}
	if s.tokens.Expiring(refreshMargin) {
		if err := s.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return s.tokens.AccessToken, nil
}

// Invalidate is called after a 401 with the token `stale`: it refreshes unless another caller already did.
func (s *TokenSource) Invalidate(ctx context.Context, stale string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead != nil {
		return s.dead
	}
	if err := s.loadLocked(); err != nil {
		return err
	}
	if s.tokens.AccessToken != stale {
		return nil // already rotated
	}
	return s.refreshLocked(ctx)
}

func (s *TokenSource) loadLocked() error {
	if s.loaded {
		return nil
	}
	t, err := s.store.Load()
	if errors.Is(err, secrets.ErrNotFound) {
		s.dead = ErrSignedOut
		return s.dead
	}
	if err != nil {
		return err
	}
	s.tokens = t
	s.loaded = true
	return nil
}

// refreshLocked makes the tokens in memory live again: by adopting a newer pair another process saved, else by
// spending the refresh token. A refusal for good (invalid_grant and kin) signs the machine out, unless a newer pair
// landed in the store while the server was answering; a transient failure keeps the tokens for a later try.
func (s *TokenSource) refreshLocked(ctx context.Context) error {
	release, err := s.store.Lock(ctx)
	if err != nil {
		return err
	}
	defer release()

	adopted := s.adoptStoredLocked()
	retried := false
	for {
		if adopted && !s.tokens.Expiring(refreshMargin) {
			return nil // another process already refreshed; nothing to spend
		}
		if s.tokens.RefreshToken == "" {
			s.dead = ErrSignedOut
			return s.dead
		}
		fresh, err := s.client.Refresh(ctx, s.clientID, s.tokens.RefreshToken)
		if err == nil {
			if fresh.Scope == "" {
				fresh.Scope = s.tokens.Scope
			}
			s.tokens = fresh
			return s.store.Save(fresh)
		}
		var te *TokenError
		if !errors.As(err, &te) || !te.Permanent() {
			return err // transient: network, 5xx; keep the old tokens
		}
		if !retried && s.adoptStoredLocked() {
			retried, adopted = true, true
			continue
		}
		s.dead = ErrSignedOut
		_ = s.store.Delete()
		return s.dead
	}
}

// adoptStoredLocked reads the store again and takes its pair when it differs from the one in memory (another process
// rotated the refresh token since this one loaded it). Reports whether the tokens changed.
func (s *TokenSource) adoptStoredLocked() bool {
	t, err := s.store.Load()
	if err != nil || t.RefreshToken == "" || t.RefreshToken == s.tokens.RefreshToken {
		return false
	}
	s.tokens = t
	return true
}

// Current returns the tokens as last loaded (no refresh), for `status` and `logout`.
func (s *TokenSource) Current() (secrets.Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return secrets.Tokens{}, err
	}
	return s.tokens, nil
}
