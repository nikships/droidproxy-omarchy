package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"
)

// publicKey is the base64 raw 32-byte ed25519 release key (same constant as
// internal/updater.PublicKey; kept here so the tool works standalone).
const publicKey = "6zisXnU+IrMiOPOAimuHxm0p9YN4VCcQfGhhUpymiCw="

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func releaseURLFor(tag string) string {
	return "https://github.com/nikships/droidproxy-omarchy/releases/tag/" + tag
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// verifySig checks a base64 ed25519 signature over data against the
// embedded public key.
// verifyFn is the verifier used by manifest; tests swap it to check
// signatures made with a test key.
var verifyFn = verifySig

func verifySig(data, sig []byte) error {
	sigB64 := trimSpaceBytes(sig)
	sigBytes, err := base64.StdEncoding.DecodeString(string(sigB64))
	if err != nil {
		return fmt.Errorf("signature is not valid base64: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("embedded public key is invalid")
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sigBytes), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, data, sigBytes) {
		return errors.New("ed25519 verification failed")
	}
	return nil
}

// verifyAgainstEmbeddededKey verifies file against file.sig (helper for the
// verify subcommand).
func verifyAgainstEmbeddededKey(path string, sig []byte) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return verifySig(data, sig)
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}
