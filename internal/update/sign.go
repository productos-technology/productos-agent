package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// Release signatures (docs/reference/agent-protocol.md, Agent versions and self-update). The release workflow signs
// checksums.txt with an Ed25519 key that only the release pipeline holds; the matching public keys are compiled into
// the agent here, so no server, mirror or download host can hand a device a binary it did not publish: the signature
// binds checksums.txt, checksums.txt binds the archive by SHA-256 and name (which carries the version). A release
// without a valid signature is refused; there is no unsigned fallback.
//
// Rotation: add the new key here, ship a release signed with the old key (devices learn the new key), then sign with
// the new one. `productos-agent-sign keygen` prints a key pair.

// PublicKeys are the raw 32-byte Ed25519 public keys, base64 (standard, padded), newest first.
var PublicKeys = []string{
	"mIF2hDoEv7MM0NWPPo1ylMgjBTfg9HYg1BxhFOqmdJA=",
}

// SignatureName is the file next to checksums.txt: one line, the 64-byte Ed25519 signature of checksums.txt's exact
// bytes, base64 (standard, padded).
const SignatureName = "checksums.txt.sig"

// ErrUnsigned is returned when no compiled-in key verifies the signature.
var ErrUnsigned = errors.New("checksums.txt is not signed by a ProductOS release key")

// ParsePublicKeys decodes base64 raw Ed25519 public keys; a malformed key is an error (a typo must never turn
// verification off silently).
func ParsePublicKeys(encoded []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(encoded))
	for _, e := range encoded {
		raw, err := base64.StdEncoding.DecodeString(e)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("malformed release public key %q", e)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	if len(keys) == 0 {
		return nil, errors.New("no release public key")
	}
	return keys, nil
}

// ParseSignature decodes the contents of checksums.txt.sig.
func ParseSignature(file []byte) ([]byte, error) {
	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(file)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%s is malformed", SignatureName)
	}
	return sig, nil
}

// VerifySignature checks checksums.txt against its signature file with any of the keys.
func VerifySignature(checksums, signatureFile []byte, keys []ed25519.PublicKey) error {
	sig, err := ParseSignature(signatureFile)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if ed25519.Verify(key, checksums, sig) {
			return nil
		}
	}
	return ErrUnsigned
}

// Sign produces the contents of checksums.txt.sig (the release pipeline's side; the agent only verifies).
func Sign(checksums []byte, private ed25519.PrivateKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, checksums)) + "\n")
}
