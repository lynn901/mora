package auth

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// TokenRecord is the resolved API token + its bound identity. Token plaintext
// is never stored — only the SHA-256 hash is looked up (design doc 06 §6.2,
// 03 §2.8 api_tokens.token_hash).
type TokenRecord struct {
	ID           string
	Name         string
	Prefix       string
	IdentityType rbac.IdentityType
	IdentityID   string
	IdentityName string
	Scope        rbac.Scope
	Groups       []string
	IsAdmin      bool
	ExpiresAt    *time.Time // nil = never expires
	RevokedAt    *time.Time // non-nil = revoked
	CreatedAt    time.Time
}

// IsValid reports whether the token is currently usable (not revoked, not expired).
func (t *TokenRecord) IsValid(now time.Time) bool {
	if t == nil {
		return false
	}
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && !t.ExpiresAt.After(now) {
		return false
	}
	return true
}

// TokenStore resolves a token hash to a token record. Implementations:
//   - MemoryTokenStore: in-memory, for tests / standalone dev.
//   - PostgresTokenStore: queries api_tokens joined to the identity tables.
type TokenStore interface {
	Lookup(ctx context.Context, tokenHash string) (*TokenRecord, error)
	// TouchLastUsed best-effort updates last_used_at. Errors are non-fatal.
	TouchLastUsed(ctx context.Context, tokenID string) error
}

// MemoryTokenStore is an in-memory TokenStore.
type MemoryTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]*TokenRecord // tokenHash -> record
}

// NewMemoryTokenStore returns an empty in-memory token store.
func NewMemoryTokenStore() *MemoryTokenStore {
	return &MemoryTokenStore{tokens: make(map[string]*TokenRecord)}
}

// Add inserts a token record keyed by its hash (for tests/dev seeding).
func (s *MemoryTokenStore) Add(tokenHash string, rec *TokenRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[tokenHash] = rec
}

// Revoke marks a token revoked (simulates instant revocation — design doc 06 §7.3).
func (s *MemoryTokenStore) Revoke(tokenHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.tokens[tokenHash]; ok {
		now := time.Now().UTC()
		rec.RevokedAt = &now
	}
}

// Lookup implements TokenStore.
func (s *MemoryTokenStore) Lookup(_ context.Context, tokenHash string) (*TokenRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.tokens[tokenHash]
	if !ok {
		return nil, nil
	}
	cp := *rec
	return &cp, nil
}

// TouchLastUsed implements TokenStore (no-op for in-memory).
func (s *MemoryTokenStore) TouchLastUsed(_ context.Context, _ string) error { return nil }

// PostgresTokenStore queries the api_tokens table (design doc 03 §2.8). It
// joins users/service_accounts to resolve the identity name and groups.
type PostgresTokenStore struct {
	pool *pgxpool.Pool
}

// NewPostgresTokenStore returns a PG-backed token store.
func NewPostgresTokenStore(pool *pgxpool.Pool) *PostgresTokenStore {
	return &PostgresTokenStore{pool: pool}
}

const tokenLookupSQL = `
SELECT t.id, t.name, t.prefix, t.identity_type, t.identity_id,
       COALESCE(u.name, sa.name, '') AS identity_name,
       COALESCE(u.email, '') AS identity_email,
       t.scope, t.expires_at, t.revoked_at, t.created_at
FROM api_tokens t
LEFT JOIN users u ON t.identity_type = 'user' AND t.identity_id = u.id
LEFT JOIN service_accounts sa ON t.identity_type = 'service_account' AND t.identity_id = sa.id
WHERE t.token_hash = $1`

// Lookup implements TokenStore. Returns (nil, nil) when the token hash is unknown.
func (s *PostgresTokenStore) Lookup(ctx context.Context, tokenHash string) (*TokenRecord, error) {
	row := s.pool.QueryRow(ctx, tokenLookupSQL, tokenHash)
	var t TokenRecord
	var identityType string
	var scope string
	var email string
	err := row.Scan(&t.ID, &t.Name, &t.Prefix, &identityType, &t.IdentityID,
		&t.IdentityName, &email, &scope, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.IdentityType = rbac.IdentityType(identityType)
	t.Scope = rbac.Scope(scope)
	t.IsAdmin = email == "admin@mora.local"
	// Groups resolution: fetch group memberships for the identity (defence in
	// depth; the Mora RBAC engine is authoritative). Left empty here as the
	// groups table is owned by the mora module; MCP relies on identity-id based
	// RBAC upstream. This can be extended with a join when groups land.
	t.Groups = nil
	return &t, nil
}

// TouchLastUsed implements TokenStore.
func (s *PostgresTokenStore) TouchLastUsed(ctx context.Context, tokenID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, tokenID)
	return err
}
