package authz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes for DelegatedManager (D5/D7 acceptance) ---

// fakeSessionRepo is an in-memory delegated_sessions store. It models the
// revocation + revision-bump contract of the real postgres SessionRepo.Revoke
// (both happen "in the same tx" — here, under one lock).
type fakeSessionRepo struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]DelegatedSession
	revisions *fakeMutableRevisionRepo // shared with the manager
}

func newFakeSessionRepo(rev *fakeMutableRevisionRepo) *fakeSessionRepo {
	return &fakeSessionRepo{
		sessions:  make(map[uuid.UUID]DelegatedSession),
		revisions: rev,
	}
}

func (f *fakeSessionRepo) Insert(_ context.Context, s DelegatedSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, id uuid.UUID) (DelegatedSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return DelegatedSession{}, errors.New("not found")
	}
	return s, nil
}

// Revoke mirrors the postgres linearization: set revoked_at AND bump the
// workspace revision in one locked step (§5.6).
func (f *fakeSessionRepo) Revoke(_ context.Context, id, workspaceID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if ok {
		now := time.Now().UTC()
		s.RevokedAt = &now
		f.sessions[id] = s
	}
	return f.revisions.bump(workspaceID), nil
}

// fakeMutableRevisionRepo is a revision store the test can both read and bump.
type fakeMutableRevisionRepo struct {
	mu   sync.Mutex
	revs map[uuid.UUID]int64
}

func newFakeMutableRevisionRepo() *fakeMutableRevisionRepo {
	return &fakeMutableRevisionRepo{revs: make(map[uuid.UUID]int64)}
}

func (f *fakeMutableRevisionRepo) Current(_ context.Context, workspaceID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revs[workspaceID], nil
}

func (f *fakeMutableRevisionRepo) bump(workspaceID uuid.UUID) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revs[workspaceID]++
	return f.revs[workspaceID]
}

// newDelegatedManager builds a DelegatedManager over the fakes with a 10s TTL
// so issue→verify has headroom inside a test.
func newDelegatedManager(t *testing.T) (*DelegatedManager, *fakeSessionRepo, *fakeMutableRevisionRepo) {
	t.Helper()
	rev := newFakeMutableRevisionRepo()
	sessions := newFakeSessionRepo(rev)
	m := NewDelegatedManager("test-secret-not-real", 10*time.Second, sessions, rev)
	return m, sessions, rev
}

// Test_Delegated_IssueThenVerify: a freshly issued token verifies and the
// claims carry the session id + revision (D5 happy path).
func Test_Delegated_IssueThenVerify(t *testing.T) {
	m, _, _ := newDelegatedManager(t)
	ws := uuid.New()
	user := uuid.New()

	token, expires, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws,
		Actions:     []domain.Action{domain.ActionUse},
		AuthzRevision: 0,
		Audience:     "mcp-server",
		ActingUserID: &user,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.True(t, expires.After(time.Now()), "expiry is in the future")

	claims, err := m.VerifyDelegated(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, ws.String(), claims.WorkspaceID)
	assert.Contains(t, claims.Actions, string(domain.ActionUse))
	assert.Equal(t, "mcp-server", claims.Audience)
}

// Test_Delegated_RevokeThenVerifyRefused: after Revoke, the same JWT is
// refused with ErrDelegatedRevoked (the row is the authority, §5.1/§5.6).
func Test_Delegated_RevokeThenVerifyRefused(t *testing.T) {
	m, sessions, _ := newDelegatedManager(t)
	ws := uuid.New()

	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Find the session id to revoke it.
	var sessionID uuid.UUID
	for id := range sessions.sessions {
		sessionID = id
	}
	_, err = m.Revoke(context.Background(), sessionID, ws)
	require.NoError(t, err)

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.ErrorIs(t, err, ErrDelegatedRevoked, "revoked session must be refused on next verify")
}

// Test_Delegated_StaleRevisionRefused: when the workspace authz revision bumps
// (a permission was yanked) AFTER issuance, VerifyDelegated returns
// ErrDelegatedStaleRevision — the §5.6 "撤权后下一次请求同步拒绝" guarantee.
func Test_Delegated_StaleRevisionRefused(t *testing.T) {
	m, sessions, rev := newDelegatedManager(t)
	ws := uuid.New()

	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Verify still passes at revision 0.
	_, err = m.VerifyDelegated(context.Background(), token)
	require.NoError(t, err)

	// Simulate an unrelated permission change bumping the revision. The session
	// row is NOT revoked, but the workspace authz moved → stale.
	rev.bump(ws)

	// The session's issued_authz_revision is still 0 (in-memory row snapshot);
	// Verify reads revisions.Current (=1) ≠ 0 → stale.
	var sessionID uuid.UUID
	for id := range sessions.sessions {
		sessionID = id
	}
	_ = sessionID

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.ErrorIs(t, err, ErrDelegatedStaleRevision, "a bumped workspace revision must invalidate the session synchronously")
}

// Test_Delegated_ForgedJTIRefused: a JWT whose session id has no row is
// refused — the row is authoritative, so a forged JTI (or one猜不出 row) does
// not pass VerifyDelegated.
func Test_Delegated_ForgedJTIRefused(t *testing.T) {
	m, _, _ := newDelegatedManager(t)
	// Issue a token, then wipe the in-memory row to simulate a forged JTI /
	// purged session.
	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: uuid.New(), Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Tamper the session id in the claims by re-signing with a different id is
	// overkill; instead, drop the row. VerifyDelegated must refuse.
	m.sessions.(*fakeSessionRepo).mu.Lock()
	for k := range m.sessions.(*fakeSessionRepo).sessions {
		delete(m.sessions.(*fakeSessionRepo).sessions, k)
	}
	m.sessions.(*fakeSessionRepo).mu.Unlock()

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.Error(t, err, "a token with no server-side row must not verify")
}

// Test_Delegated_TTLClamped: a caller asking for >30s gets clamped to
// DelegatedTTL (§5.6: ≤30s).
func Test_Delegated_TTLClamped(t *testing.T) {
	rev := newFakeMutableRevisionRepo()
	sessions := newFakeSessionRepo(rev)
	m := NewDelegatedManager("s", 2*time.Minute, sessions, rev)
	if m.ttl != DelegatedTTL {
		t.Fatalf("ttl clamped to DelegatedTTL, got %v", m.ttl)
	}
}

// Test_Delegated_IssueValidatesInput: missing workspace_id or audience is
// rejected before any row is written.
func Test_Delegated_IssueValidatesInput(t *testing.T) {
	m, _, _ := newDelegatedManager(t)
	_, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: uuid.Nil, Audience: "mcp-server",
	})
	assert.Error(t, err, "nil workspace must be rejected")

	_, _, err = m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: uuid.New(), Audience: "",
	})
	assert.Error(t, err, "empty audience must be rejected")
}

// Test_Delegated_BadSignatureRefused: a token signed with a different secret
// is refused at the signature step.
func Test_Delegated_BadSignatureRefused(t *testing.T) {
	m, _, _ := newDelegatedManager(t)
	// Build a second manager with a DIFFERENT secret and issue through it,
	// then verify through m — signature must fail.
	rev := newFakeMutableRevisionRepo()
	other := NewDelegatedManager("other-secret", 10*time.Second, newFakeSessionRepo(rev), rev)
	token, _, err := other.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: uuid.New(), Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.Error(t, err, "a token signed with a foreign secret must not verify")
}

// Ensure the fakeSessionRepo satisfies the interface.
var _ SessionRepo = (*fakeSessionRepo)(nil)
