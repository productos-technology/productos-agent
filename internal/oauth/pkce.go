package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewVerifier returns a 64-character PKCE verifier (RFC 7636 section 4.1, unreserved characters only).
func NewVerifier() (string, error) {
	raw := make([]byte, 48) // 48 bytes -> 64 base64url chars, within 43..128
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Challenge is BASE64URL(SHA256(verifier)) without padding (the S256 method).
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState returns an unguessable `state` value.
func NewState() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
