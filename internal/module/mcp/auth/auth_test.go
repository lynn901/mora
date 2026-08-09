package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/platform/rbac"
)

func TestHashTokenStableAndLowercase(t *testing.T) {
	h1 := HashToken("mora_abc123")
	h2 := HashToken("mora_abc123")
	assert.Equal(t, h1, h2, "hash must be deterministic")
	assert.Len(t, h1, 64, "sha256 hex is 64 chars")
	// Different input → different hash.
	assert.NotEqual(t, h1, HashToken("mora_other"))
}

func TestExtractBearer(t *testing.T) {
	tok, err := ExtractBearer("Bearer mora_abc")
	require.NoError(t, err)
	assert.Equal(t, "mora_abc", tok)

	tok, err = ExtractBearer("mora_bare")
	require.NoError(t, err)
	assert.Equal(t, "mora_bare", tok)

	_, err = ExtractBearer("")
	assert.Error(t, err)

	_, err = ExtractBearer("Basic dXNlcjpwYXNz")
	assert.Error(t, err)
}

func TestTokenRecordIsValid(t *testing.T) {
	now := time.Now().UTC()
	valid := &TokenRecord{Scope: rbac.ScopeReadOnly, CreatedAt: now}
	assert.True(t, valid.IsValid(now))

	// Revoked.
	revoked := &TokenRecord{Scope: rbac.ScopeReadOnly, CreatedAt: now, RevokedAt: &now}
	assert.False(t, revoked.IsValid(now))

	// Expired.
	exp := now.Add(-time.Hour)
	expired := &TokenRecord{Scope: rbac.ScopeReadOnly, CreatedAt: exp, ExpiresAt: &exp}
	assert.False(t, expired.IsValid(now))

	// Not yet expired.
	future := now.Add(time.Hour)
	notExpired := &TokenRecord{Scope: rbac.ScopeReadOnly, ExpiresAt: &future}
	assert.True(t, notExpired.IsValid(now))

	// Nil record.
	var nilRec *TokenRecord
	assert.False(t, nilRec.IsValid(now))
}

func TestScopeAllowsWrite(t *testing.T) {
	assert.False(t, rbac.ScopeReadOnly.AllowsWrite())
	assert.True(t, rbac.ScopeReadWrite.AllowsWrite())
	assert.True(t, rbac.ScopeAdmin.AllowsWrite())
}

func TestCheckWriteScope(t *testing.T) {
	rw := &AuthContext{Scope: rbac.ScopeReadWrite}
	ro := &AuthContext{Scope: rbac.ScopeReadOnly}
	assert.NoError(t, CheckWriteScope(rw))
	assert.Error(t, CheckWriteScope(ro))
	assert.Error(t, CheckWriteScope(nil))
}

func TestMemoryTokenStoreLookup(t *testing.T) {
	store := NewMemoryTokenStore()
	hash := HashToken("mora_lookup")
	store.Add(hash, &TokenRecord{
		ID: "t1", Name: "n", IdentityType: rbac.IdentityUser, IdentityID: "u1",
		Scope: rbac.ScopeReadOnly,
	})
	rec, err := store.Lookup(context.Background(), hash)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "t1", rec.ID)
	assert.Equal(t, "u1", rec.IdentityID)

	// Unknown hash → nil, nil.
	rec, err = store.Lookup(context.Background(), HashToken("nope"))
	require.NoError(t, err)
	assert.Nil(t, rec)

	// Revoke → invalid.
	store.Revoke(hash)
	rec, err = store.Lookup(context.Background(), hash)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, rec.IsValid(time.Now().UTC()))
}

func TestMemoryRateLimiter(t *testing.T) {
	rl := NewMemoryRateLimiter()
	ctx := context.Background()
	// Limit 3/min.
	for i := 0; i < 3; i++ {
		d, err := rl.Allow(ctx, "tok-1", BucketRead, 3)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "call %d should be allowed", i)
	}
	// 4th call rejected.
	d, err := rl.Allow(ctx, "tok-1", BucketRead, 3)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.GreaterOrEqual(t, d.RetryAfter, time.Duration(0))

	// Different token has its own bucket.
	d, err = rl.Allow(ctx, "tok-2", BucketRead, 3)
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	// Write bucket is independent of read bucket for the same token.
	d, err = rl.Allow(ctx, "tok-1", BucketWrite, 3)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "write bucket is separate from read bucket")
}
