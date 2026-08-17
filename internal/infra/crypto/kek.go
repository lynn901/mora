// Package crypto implements the envelope-encryption adapters for the Phase 4
// evidence store (design-docs/18 §4.2, decision D4; evidence.KEK +
// evidence.Crypto ports).
//
// Split of responsibilities (§4.2):
//   - Crypto (AEAD): content-level AES-256-GCM over a per-evidence DEK. The
//     DEK is a random 256-bit key generated per evidence row; it never
//     persists in plaintext. Encrypt returns ciphertext + nonce.
//   - KEK (envelope): wraps the DEK under a versioned master key, returning
//     the wrapped bytes + the version. Reads unwrap by the version the
//     ciphertext carries — rotation never rewrites existing ciphertext;
//     new writes always use the current version (D4 密钥轮换).
//
// The KEK plaintext is injected from config (MORA_EVIDENCE_KEK) and held
// in-memory only — never logged, never persisted, never returned by any API.
// An empty/invalid KEK surfaces as an error on first use (07-security: no
// hardcoded keys; production MUST inject). Key rotation is a deploy concern
// (new env var value + bumped version) — the adapter tracks versions so old
// ciphertext stays decryptable.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// ErrKEKNotConfigured is returned when the KEK was never injected. Production
// MUST inject MORA_EVIDENCE_KEK; an empty KEK fails closed (07-security).
var ErrKEKNotConfigured = errors.New("memory: evidence KEK not configured")

// ErrInvalidKEK is returned when the injected KEK is not a valid 32-byte
// (256-bit) AES key after decoding (hex or base64 forms accepted).
var ErrInvalidKEK = errors.New("memory: evidence KEK must be 32 bytes (hex or base64)")

// currentKEKVersion is the envelope-encryption version this build writes. A
// deploy that rotates the KEK bumps this (along with the injected key
// material) so reads of old ciphertext still unwrap by the stored version
// (D4: rotation does not rewrite existing ciphertext). Phase 4 first version.
const currentKEKVersion = 1

// EnvelopeKEK is the default evidence.KEK adapter: a single in-memory master
// key that wraps per-evidence DEKs under AES-256-GCM. Version is tracked so a
// future rotation (new version + new key material) can still unwrap old wrapped
// DEKs — callers persist the version alongside the ciphertext (§4.2).
type EnvelopeKEK struct {
	mu      sync.RWMutex
	keys    map[int][]byte // version → 32-byte master key
	current int
}

// NewEnvelopeKEK builds an EnvelopeKEK from the injected key material. The
// key may be hex- or base64-encoded, or raw 32-byte bytes (UTF-8). An empty
// rawKEK returns a KEK that fails closed on first use — callers MUST check
// Health/CurrentVersion before writes.
func NewEnvelopeKEK(rawKEK string) (*EnvelopeKEK, error) {
	k := &EnvelopeKEK{keys: make(map[int][]byte), current: currentKEKVersion}
	if rawKEK == "" {
		// Not configured — writes will fail with ErrKEKNotConfigured. Return no
		// error here so wiring is unconditional; the failure is surfaced at use.
		return k, nil
	}
	key, err := decodeKey(rawKEK)
	if err != nil {
		return nil, err
	}
	if err := validateAES256Key(key); err != nil {
		return nil, err
	}
	k.keys[currentKEKVersion] = key
	return k, nil
}

// Rotate installs a new key material as the current version. The previous key
// is retained under its old version so reads of existing wrapped DEKs still
// unwrap (D4: rotation does not rewrite existing ciphertext). Callers bump
// the version monotonically; a lower-or-equal version is rejected.
func (k *EnvelopeKEK) Rotate(version int, rawKEK string) error {
	if version <= k.current {
		return fmt.Errorf("memory: kek rotation version %d must exceed current %d", version, k.current)
	}
	key, err := decodeKey(rawKEK)
	if err != nil {
		return err
	}
	if err := validateAES256Key(key); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[version] = key
	k.current = version
	return nil
}

// Wrap encrypts a per-evidence DEK under the current KEK version (§4.2). The
// returned bytes are nonce||ciphertext under AES-256-GCM, and the version is
// the one callers persist alongside memory_evidence.key_version.
func (k *EnvelopeKEK) Wrap(ctx context.Context, dek []byte) ([]byte, int, error) {
	k.mu.RLock()
	master, ok := k.keys[k.current]
	ver := k.current
	k.mu.RUnlock()
	if !ok {
		return nil, 0, ErrKEKNotConfigured
	}
	gcm, err := aesGCM(master)
	if err != nil {
		return nil, 0, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, err
	}
	wrapped := gcm.Seal(nil, nonce, dek, nil)
	out := make([]byte, 0, len(nonce)+len(wrapped))
	out = append(out, nonce...)
	out = append(out, wrapped...)
	return out, ver, nil
}

// Unwrap recovers the DEK wrapped under the given version. A missing version
// (rotated away or never installed) returns ErrKEKNotConfigured so the caller
// surfaces it as a denial — the plaintext is not recoverable (fail closed).
func (k *EnvelopeKEK) Unwrap(ctx context.Context, wrapped []byte, version int) ([]byte, error) {
	k.mu.RLock()
	master, ok := k.keys[version]
	k.mu.RUnlock()
	if !ok {
		return nil, ErrKEKNotConfigured
	}
	gcm, err := aesGCM(master)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(wrapped) < ns {
		return nil, fmt.Errorf("memory: wrapped DEK too short (got %d, need nonce %d)", len(wrapped), ns)
	}
	nonce, ct := wrapped[:ns], wrapped[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}

// CurrentVersion returns the active KEK version (for new writes). A KEK that
// was never configured returns ErrKEKNotConfigured — callers MUST surface this
// before attempting a write (07-security: fail closed, no default key).
func (k *EnvelopeKEK) CurrentVersion(ctx context.Context) (int, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if _, ok := k.keys[k.current]; !ok {
		return 0, ErrKEKNotConfigured
	}
	return k.current, nil
}

// Compile-time check: EnvelopeKEK satisfies evidence.KEK.
var _ evidence.KEK = (*EnvelopeKEK)(nil)

// AESGCM is the default evidence.Crypto adapter: content-level AES-256-GCM
// over a per-evidence DEK. It is stateless — the DEK is supplied per call —
// so a single AESGCM serves all evidence rows. The nonce is returned for
// callers to persist alongside the ciphertext.
type AESGCM struct{}

// NewAESGCM builds the content-level AEAD adapter.
func NewAESGCM() *AESGCM { return &AESGCM{} }

// Encrypt produces ciphertext + nonce under the given DEK (§4.2). The DEK
// MUST be a valid 32-byte AES-256 key; an invalid key returns ErrInvalidKEK.
func (a *AESGCM) Encrypt(ctx context.Context, dek, plaintext []byte) ([]byte, []byte, error) {
	if err := validateAES256Key(dek); err != nil {
		return nil, nil, err
	}
	gcm, err := aesGCM(dek)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	return ct, nonce, nil
}

// Decrypt reverses Encrypt. A tampered/short ciphertext returns an error so
// the read path surfaces a denial (fail closed, §4.3 read chain).
func (a *AESGCM) Decrypt(ctx context.Context, dek, ciphertext, nonce []byte) ([]byte, error) {
	if err := validateAES256Key(dek); err != nil {
		return nil, err
	}
	gcm, err := aesGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("memory: nonce size mismatch (got %d, want %d)", len(nonce), gcm.NonceSize())
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// Compile-time check: AESGCM satisfies evidence.Crypto.
var _ evidence.Crypto = (*AESGCM)(nil)

// aesGCM builds a GCM mode cipher from a 32-byte key.
func aesGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// validateAES256Key returns ErrInvalidKEK unless key is 32 bytes.
func validateAES256Key(key []byte) error {
	if len(key) != 32 {
		return ErrInvalidKEK
	}
	return nil
}

// decodeKey accepts hex, base64 (std or URL-safe), or raw 32-byte strings.
// The config MORA_EVIDENCE_KEK may be any of these forms; the deploy chooses.
func decodeKey(raw string) ([]byte, error) {
	// Raw 32-byte string (least likely, but accept).
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	// Hex (64 chars → 32 bytes).
	if b, err := hex.DecodeString(raw); err == nil {
		return b, nil
	}
	// Base64 std.
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	// Base64 URL-safe.
	if b, err := base64.URLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	return nil, ErrInvalidKEK
}
