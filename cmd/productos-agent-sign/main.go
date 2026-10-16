// productos-agent-sign is the release pipeline's signer (standard library only; .goreleaser.yaml runs it with
// `go run`): it signs checksums.txt with the Ed25519 private key in an environment variable and writes
// checksums.txt.sig, the file the agent and install.sh verify against the public keys compiled into
// internal/update/sign.go.
//
//	productos-agent-sign keygen                                    # prints a new key pair (store the private key as a secret)
//	productos-agent-sign sign --key-env AGENT_SIGNING_KEY --out checksums.txt.sig checksums.txt
//	productos-agent-sign verify --public <base64> --sig checksums.txt.sig checksums.txt
//
// Keys are base64 (standard, padded): the private key is the 32-byte Ed25519 seed, the public key the 32-byte key.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/productos-technology/productos-agent/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "productos-agent-sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: productos-agent-sign keygen | sign --key-env NAME --out FILE INPUT | verify --public KEY --sig FILE INPUT")
	os.Exit(2)
}

// keygen prints the public key (for internal/update/sign.go and install.sh) on stdout and the private key on the
// descriptor named by PRODUCTOS_SIGN_PRIVATE_OUT (a file path, created 0600), else on stdout too.
func keygen() error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pub := base64.StdEncoding.EncodeToString(public)
	priv := base64.StdEncoding.EncodeToString(private.Seed())
	fmt.Printf("public_key_base64: %s\n", pub)
	fmt.Printf("public_key_pem:\n%s", publicPEM(public))
	if out := os.Getenv("PRODUCTOS_SIGN_PRIVATE_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(priv+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("private_key: written to %s (mode 0600)\n", out)
		return nil
	}
	fmt.Printf("private_key_base64: %s\n", priv)
	return nil
}

// publicPEM is the SubjectPublicKeyInfo form openssl reads (`openssl pkeyutl -verify -pubin -inkey pub.pem -rawin`).
func publicPEM(public ed25519.PublicKey) string {
	prefix := []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: append(prefix, public...)}))
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyEnv := fs.String("key-env", "AGENT_SIGNING_KEY", "environment variable holding the base64 private key seed")
	out := fs.String("out", "", "signature file to write (default: INPUT.sig)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("sign takes one input file")
	}
	input := fs.Arg(0)
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv(*keyEnv)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("%s does not hold a base64 Ed25519 seed", *keyEnv)
	}
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	target := *out
	if target == "" {
		target = input + ".sig"
	}
	return os.WriteFile(target, update.Sign(data, ed25519.NewKeyFromSeed(seed)), 0o644)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	public := fs.String("public", "", "base64 public key (default: the keys compiled into the agent)")
	sigFile := fs.String("sig", "", "signature file (default: INPUT.sig)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("verify takes one input file")
	}
	input := fs.Arg(0)
	encoded := update.PublicKeys
	if *public != "" {
		encoded = []string{*public}
	}
	keys, err := update.ParsePublicKeys(encoded)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	target := *sigFile
	if target == "" {
		target = input + ".sig"
	}
	sig, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if err := update.VerifySignature(data, sig, keys); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
