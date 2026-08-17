// Package crypto implements the Phase 4 evidence envelope-encryption
// primitives (design-docs/18 §4.2, decision D4): an AES-256-GCM AEAD over a
// per-evidence data-encryption key (DEK), and a default env-backed KEK that
// wraps/unwraps the DEK by version. A rotated KEK (new version) does NOT
// rewrite existing ciphertext — reads unwrap by the version the ciphertext
// carries; writes always wrap with the current version.
//
// This package holds ONLY the crypto primitives + a dev/local KEK. Production
// KEK management (KMS / Secret manager) is the deploy engineer's wiring
// (design-docs/18 §12.2); the evidence.KEK port lets a real KMS adapter drop
// in without touching callers.
//
// Security: no key material is logged or returned in plaintext. The DEK is
// generated with crypto/rand per evidence and lives only in-memory between
// Wrap and the caller's Encrypt. The KEK plaintext is held in the EnvKEK
// struct and never serialized by String() (07-security §10: no in-code/in-log
// secrets).
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// AESGCM is the evidence.Crypto port over a DEK: AES-256-GCM AEAD. The nonce
// is generated per Encrypt with crypto/rand and returned to the caller to
// persist alongside the ciphertext (the DB row stores encrypted_content only;
// the nonce is prepended to the ciphertext by this implementation so callers
// store a single BYTEA — see Seal/Open below).
type AESGCM struct{}

// NewAESGCM returns an AES-256-GCM Crypto.
func NewAESGCM() *AESGCM { return &AESGCM{} }

// Encrypt produces ciphertext (with nonce prepended) under the given 32-byte
// DEK. The nonce is 96 bits (GCM standard) and is NOT reused — each call draws
// fresh randomness from crypto/rand.
func (c *AESGCM) Encrypt(ctx context.Context, dek, plaintext []byte) ([]byte, []byte, error) {
	g, err := newGCM(dek)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("aesgcm: nonce: %w", err)
	}
	// Seal appends the tag to dst; we prepend the nonce so the stored BYTEA is
	// self-describing (nonce || ciphertext).
	ct := g.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ct...), nonce, nil
}

// Decrypt reverses Encrypt when the caller passes the full nonce||ciphertext
// blob as ciphertext and the matching nonce. (Callers that store the blob in a
// single column use OpenBlob below; this method satisfies the evidence.Crypto
// contract for callers that kept the nonce separately.)
func (c *AESGCM) Decrypt(ctx context.Context, dek, ciphertext, nonce []byte) ([]byte, error) {
	g, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ciphertext, nil)
}

// Seal returns nonce||ciphertext as one blob, for callers that store a single
// BYTEA column (the memory_evidence.encrypted_content shape). This is the
// primary write path; OpenBlob is the read path.
func (c *AESGCM) Seal(dek, plaintext []byte) ([]byte, error) {
	g, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("aesgcm: nonce: %w", err)
	}
	ct := g.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ct...), nil
}

// OpenBlob reverses Seal: it splits the nonce from the blob and decrypts.
func (c *AESGCM) OpenBlob(dek, blob []byte) ([]byte, error) {
	g, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	ns := g.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("aesgcm: ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return g.Open(nil, nonce, ct, nil)
}

func newGCM(dek []byte) (cipher.AEAD, error) {
	if len(dek) != 32 {
		return nil, fmt.Errorf("aesgcm: DEK must be 32 bytes (AES-256), got %d", len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: new cipher: %w", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: new gcm: %w", err)
	}
	return g, nil
}

// Compile-time check: AESGCM satisfies evidence.Crypto.
var _ evidence.Crypto = (*AESGCM)(nil)

// EnvKEK is a local/env-backed envelope key-encryption key. It holds one or
// more 32-byte KEK versions keyed by integer version number; Wrap always uses
// the current (highest) version, Unwrap looks up the version the ciphertext
// carries. Rotation = add a new version; old ciphertext stays readable.
//
// This is the dev/local adapter. A production KMS adapter implements the same
// evidence.KEK port with real key material (deploy engineer's wiring, §12.2).
// The KEK plaintext is never returned by any method.
type EnvKEK struct {
	versions map[int][]byte
	current  int
}

// NewEnvKEK builds a KEK from a {version: key} map. The highest version is the
// active one for new writes. Keys must be 32 bytes (AES-256).
func NewEnvKEK(versions map[int][]byte) (*EnvKEK, error) {
	if len(versions) == 0 {
		return nil, errors.New("kek: no versions configured")
	}
	for v, k := range versions {
		if len(k) != 32 {
			return nil, fmt.Errorf("kek: version %d key must be 32 bytes, got %d", v, len(k))
		}
	}
	cur := 0
	for v := range versions {
		if v > cur {
			cur = v
		}
	}
	return &EnvKEK{versions: versions, current: cur}, nil
}

// NewEnvKEKFromEnv reads the default KEK from MORA_EVIDENCE_KEK (base64/hex
// optional; raw 32 bytes acceptable) and registers it as version 1. A missing
// env var returns an error — production MUST inject it (07-security: no
// hardcoded keys). This is the convenience constructor for single-version
// local dev; multi-version rotation uses NewEnvKEK directly.
func NewEnvKEKFromEnv() (*EnvKEK, error) {
	raw := os.Getenv("MORA_EVIDENCE_KEK")
	if len(raw) != 32 {
		return nil, fmt.Errorf("kek: MORA_EVIDENCE_KEK must be 32 bytes, got %d", len(raw))
	}
	return NewEnvKEK(map[int][]byte{1: []byte(raw)})
}

// Wrap encrypts a DEK under the current KEK version, returning the wrapped
// bytes + the version to persist alongside the ciphertext.
func (k *EnvKEK) Wrap(ctx context.Context, dek []byte) ([]byte, int, error) {
	key := k.versions[k.current]
	g, err := newGCM(key)
	if err != nil {
		return nil, 0, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("kek: nonce: %w", err)
	}
	wrapped := g.Seal(nil, nonce, dek, nil)
	return append(nonce, wrapped...), k.current, nil
}

// Unwrap recovers the DEK wrapped under the given version. A missing version
// returns an error so a reader knows the KEK for that version is unavailable
// (the row stays encrypted; the caller surfaces a leak-safe deny, §9.3).
func (k *EnvKEK) Unwrap(ctx context.Context, wrapped []byte, version int) ([]byte, error) {
	key, ok := k.versions[version]
	if !ok {
		return nil, fmt.Errorf("kek: version %d not available", version)
	}
	g, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := g.NonceSize()
	if len(wrapped) < ns {
		return nil, errors.New("kek: wrapped key too short")
	}
	nonce, ct := wrapped[:ns], wrapped[ns:]
	return g.Open(nil, nonce, ct, nil)
}

// CurrentVersion returns the active KEK version (for new writes).
func (k *EnvKEK) CurrentVersion(ctx context.Context) (int, error) {
	return k.current, nil
}

// Compile-time check: EnvKEK satisfies evidence.KEK.
var _ evidence.KEK = (*EnvKEK)(nil)

// GenerateDEK returns a fresh 32-byte AES-256 DEK from crypto/rand. It is the
// per-evidence data key the caller wraps with the KEK and feeds to the AEAD.
func GenerateDEK() ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("dek: rand: %w", err)
	}
	return dek, nil
}
