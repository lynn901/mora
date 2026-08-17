package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/lynn901/mora/internal/module/memory/evidence"
)

func TestNewEnvelopeKEK_FailClosedWhenEmpty(t *testing.T) {
	t.Parallel()
	k, err := NewEnvelopeKEK("")
	if err != nil {
		t.Fatalf("empty KEK should build (fail-at-use), got error: %v", err)
	}
	if _, err := k.CurrentVersion(context.Background()); !errors.Is(err, ErrKEKNotConfigured) {
		t.Fatalf("expected ErrKEKNotConfigured, got %v", err)
	}
	if _, _, err := k.Wrap(context.Background(), []byte("32-bytes-dek-must-be-exactly-32!!")); !errors.Is(err, ErrKEKNotConfigured) {
		t.Fatalf("Wrap should fail closed, got %v", err)
	}
}

func TestNewEnvelopeKEK_RejectsInvalidKey(t *testing.T) {
	t.Parallel()
	if _, err := NewEnvelopeKEK("too-short"); !errors.Is(err, ErrInvalidKEK) {
		t.Fatalf("expected ErrInvalidKEK, got %v", err)
	}
}

func TestEnvelopeKEK_RoundTripAndVersion(t *testing.T) {
	t.Parallel()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	k, err := NewEnvelopeKEK(key)
	if err != nil {
		t.Fatalf("NewEnvelopeKEK: %v", err)
	}
	ver, err := k.CurrentVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if ver != 1 {
		t.Fatalf("expected version 1, got %d", ver)
	}
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	wrapped, wVer, err := k.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if wVer != ver {
		t.Fatalf("wrap version %d != current %d", wVer, ver)
	}
	rec, err := k.Unwrap(context.Background(), wrapped, wVer)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if string(rec) != string(dek) {
		t.Fatal("unwrapped DEK != original")
	}
}

func TestEnvelopeKEK_RotationKeepsOldVersion(t *testing.T) {
	t.Parallel()
	old := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	new := base64.StdEncoding.EncodeToString([]byte("98765432109876543210987654321098"))
	k, _ := NewEnvelopeKEK(old)
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	wrapped, v1, _ := k.Wrap(context.Background(), dek)
	// Rotate to version 2.
	if err := k.Rotate(2, new); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if v, _ := k.CurrentVersion(context.Background()); v != 2 {
		t.Fatalf("current should be 2, got %d", v)
	}
	// Old ciphertext still unwraps under version 1 (D4: rotation does not
	// rewrite existing ciphertext).
	rec, err := k.Unwrap(context.Background(), wrapped, v1)
	if err != nil {
		t.Fatalf("Unwrap old version after rotation: %v", err)
	}
	if string(rec) != string(dek) {
		t.Fatal("old wrapped DEK mismatch after rotation")
	}
	// New writes use version 2.
	_, v2, _ := k.Wrap(context.Background(), dek)
	if v2 != 2 {
		t.Fatalf("new write should use version 2, got %d", v2)
	}
	// Rotation to a non-increasing version is rejected.
	if err := k.Rotate(2, new); err == nil {
		t.Fatal("rotation to same version should be rejected")
	}
	// Missing version unwraps as fail-closed.
	if _, err := k.Unwrap(context.Background(), wrapped, 999); !errors.Is(err, ErrKEKNotConfigured) {
		t.Fatalf("missing version should return ErrKEKNotConfigured, got %v", err)
	}
}

func TestAESGCM_RoundTrip(t *testing.T) {
	t.Parallel()
	a := NewAESGCM()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	plain := []byte("mora evidence: 决策 mora-api 监听 :8990")
	ct, nonce, err := a.Encrypt(context.Background(), dek, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(ct) == string(plain) {
		t.Fatal("ciphertext equals plaintext")
	}
	rec, err := a.Decrypt(context.Background(), dek, ct, nonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(rec) != string(plain) {
		t.Fatal("decrypted != original")
	}
}

func TestAESGCM_TamperFailsClosed(t *testing.T) {
	t.Parallel()
	a := NewAESGCM()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	ct, nonce, _ := a.Encrypt(context.Background(), dek, []byte("secret"))
	ct[0] ^= 0xff
	if _, err := a.Decrypt(context.Background(), dek, ct, nonce); err == nil {
		t.Fatal("tampered ciphertext must fail decryption")
	}
}

func TestAESGCM_RejectsShortDEK(t *testing.T) {
	t.Parallel()
	a := NewAESGCM()
	if _, _, err := a.Encrypt(context.Background(), []byte("short"), []byte("x")); !errors.Is(err, ErrInvalidKEK) {
		t.Fatalf("expected ErrInvalidKEK, got %v", err)
	}
}

// Compile-time: ensure the adapters satisfy the evidence ports.
var (
	_ evidence.KEK    = (*EnvelopeKEK)(nil)
	_ evidence.Crypto = (*AESGCM)(nil)
)
