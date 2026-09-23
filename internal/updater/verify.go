package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os/exec"
	"syscall"
)

// PublicKey is the base64 raw 32-byte ed25519 public key every release
// signature must verify against. PLACEHOLDER — replace with the real key
// (tools/release keygen prints it).
const PublicKey = "6zisXnU+IrMiOPOAimuHxm0p9YN4VCcQfGhhUpymiCw="

// defaultVerifier is the real ed25519 verification.
func defaultVerifier(pubKey, message, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubKey), message, sig) {
		return errors.New("ed25519 verification failed")
	}
	return nil
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(stringsTrimSpace(s))
}

func stringsTrimSpace(s string) string {
	b := []byte(s)
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return string(b[start:end])
}

// hasher computes both the hex SHA-256 (for latest.json) and keeps the raw
// bytes available for signature verification, in one pass over the stream.
type hasher struct {
	h hash.Hash
}

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (h *hasher) Write(p []byte) { h.h.Write(p) }

// Sum returns the hex-encoded SHA-256 of the bytes written so far.
func (h *hasher) Sum() string { return hex.EncodeToString(h.h.Sum(nil)) }

// digest returns a copy of the raw SHA-256 digest (what ed25519 signs
// indirectly — the signature is over the file bytes, so verification
// recomputes from the same digest the file would hash to).
func (h *hasher) digest() []byte {
	sum := h.h.Sum(nil)
	cp := make([]byte, len(sum))
	copy(cp, sum)
	return cp
}

// ---- process hooks (injectable in tests) ------------------------------------

func newCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func syscallExec(argv0 string, argv []string, envv []string) error {
	return syscall.Exec(argv0, argv, envv)
}
