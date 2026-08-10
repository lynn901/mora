package handler

// middleware_matrix_test.go is the Phase 0 authorization test matrix entry
// for §8.2 use case 8 (design-docs/13 §8.2):
//
//	"MCP 内部调用缺 delegated context，仅有服务身份 → 降级为 service account
//	 受限权限，不 fallback admin。"
//
// AuthMiddleware (middleware.go) consumes the delegated context per §4.4:
//   - INTERNAL_SERVICE_TOKEN alone ⇒ trusted service identity, restricted
//     service_account capability, IsAdmin=false (no admin fallback).
//   - a delegated JWT ⇒ DelegatedManager.VerifyDelegated resolves the acting
//     principal (UserID/AgentID/WorkspaceID/Actions); IsAdmin comes only from
//     the claims, never a client-set header.
//   - X-Identity-Id / X-Identity-Admin are deprecated (read-and-ignored).
//
// These tests assert the new behavior against the real DelegatedManager over
// in-memory fakes (the fakes mirror authz/delegated_test.go's contract:
// Insert/Get/Revoke + a bumpable revision store).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/auth"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- in-memory fakes for authz.SessionRepo / authz.RevisionRepo ---
// They mirror the contract of internal/infra/postgres/authz_repos.go's
// sessionRepo / revisionsRepo (revocation + revision-bump in one locked step,
// §5.6 linearization) so the middleware exercises the real VerifyDelegated
// path without a database.

type mmSessionRepo struct {
	mu        sync.Mutex
	sessions  map[uuid.UUID]authz.DelegatedSession
	revisions *mmRevisionRepo
}

func newMMSessionRepo(rev *mmRevisionRepo) *mmSessionRepo {
	return &mmSessionRepo{sessions: make(map[uuid.UUID]authz.DelegatedSession), revisions: rev}
}

func (f *mmSessionRepo) Insert(_ context.Context, s authz.DelegatedSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
	return nil
}

func (f *mmSessionRepo) Get(_ context.Context, id uuid.UUID) (authz.DelegatedSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return authz.DelegatedSession{}, errors.New("not found")
	}
	return s, nil
}

func (f *mmSessionRepo) Revoke(_ context.Context, id, workspaceID uuid.UUID) (int64, error) {
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

type mmRevisionRepo struct {
	mu   sync.Mutex
	revs map[uuid.UUID]int64
}

func newMMRevisionRepo() *mmRevisionRepo {
	return &mmRevisionRepo{revs: make(map[uuid.UUID]int64)}
}

func (f *mmRevisionRepo) Current(_ context.Context, workspaceID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revs[workspaceID], nil
}

func (f *mmRevisionRepo) bump(workspaceID uuid.UUID) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revs[workspaceID]++
	return f.revs[workspaceID]
}

// newMMDelegatedManager builds a real DelegatedManager over the in-memory
// fakes with a 10s TTL so issue→verify has headroom inside a test.
func newMMDelegatedManager() (*authz.DelegatedManager, *mmSessionRepo, *mmRevisionRepo) {
	rev := newMMRevisionRepo()
	sessions := newMMSessionRepo(rev)
	return authz.NewDelegatedManager("mm-test-secret", 10*time.Second, sessions, rev), sessions, rev
}

// probeRecorder mounts AuthMiddleware + a handler that records the resolved
// AuthState, returning the recorder and a pointer to the captured state.
func probeRecorder(t *testing.T, tm *auth.TokenManager, internal string, dm *authz.DelegatedManager) (*gin.Engine, *AuthState) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var got AuthState
	r := gin.New()
	r.Use(AuthMiddleware(tm, internal, dm))
	r.GET("/probe", func(c *gin.Context) {
		got = MustAuth(c)
		c.Status(http.StatusOK)
	})
	return r, &got
}

// Test_UseCase8_NoDelegatedContextDoesNotFallbackAdmin: an MCP internal call
// carrying ONLY the INTERNAL_SERVICE_TOKEN (no delegated JWT, no trusted
// X-Identity-* headers) must NOT be treated as admin. Per §8.2 UC8 it degrades
// to a service_account with restricted permissions.
func Test_UseCase8_NoDelegatedContextDoesNotFallbackAdmin(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	r, got := probeRecorder(t, tm, internal, dm)

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+internal)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "internal-token bearer must authenticate as a trusted service")
	assert.False(t, got.IsAdmin, "internal call without delegated context must NOT fall back to admin (§4.4)")
	assert.True(t, got.IsServiceCaller, "must be marked as an internal service caller")
	assert.Equal(t, domain.SubjectServiceAccount, got.SubjectType, "must degrade to service_account identity")
	assert.Equal(t, uuid.Nil, got.UserID, "no delegated context ⇒ no end-principal resolved")
}

// Test_UseCase8_DeprecatedHeadersIgnored: even if a caller still sends the
// legacy X-Identity-Id / X-Identity-Admin headers alongside the internal
// service token (no delegated JWT), those headers are NOT trusted — IsAdmin
// stays false and the claimed identity is NOT adopted. This is the §4.4 step 3
// deprecation: X-Identity-* no longer represents authority.
func Test_UseCase8_DeprecatedHeadersIgnored(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	r, got := probeRecorder(t, tm, internal, dm)

	forgedUser := uuid.New().String()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+internal)
	req.Header.Set("X-Identity-Id", forgedUser) // deprecated — must be ignored
	req.Header.Set("X-Identity-Admin", "true")  // deprecated — must NOT grant admin

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, got.IsAdmin, "X-Identity-Admin must not be trusted (deprecated)")
	assert.Equal(t, uuid.Nil, got.UserID, "X-Identity-Id must not be adopted as the principal (deprecated)")
	assert.True(t, got.IsServiceCaller)
	assert.Equal(t, domain.SubjectServiceAccount, got.SubjectType, "degraded to service_account, not the forged user")
}

// Test_UseCase8_DelegatedContextResolvesActingPrincipal: when an internal call
// carries a valid delegated JWT, the resolved AuthState reflects the acting
// principal from the delegated claims (UserID=acting_user_id, WorkspaceID,
// Actions), NOT admin and NOT a client header. This is the §4.4 step 2 happy
// path once the delegated-context wiring is complete.
func Test_UseCase8_DelegatedContextResolvesActingPrincipal(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	ws := uuid.New()
	actingUser := uuid.New()
	actions := []domain.Action{domain.ActionUse, domain.ActionRead}

	token, _, err := dm.IssueDelegated(context.Background(), authz.DelegatedRequest{
		ActingUserID:  &actingUser,
		WorkspaceID:   ws,
		Actions:       actions,
		AuthzRevision: 0,
		Audience:      "mcp-server",
	})
	require.NoError(t, err)

	r, got := probeRecorder(t, tm, internal, dm)

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token) // delegated JWT, not the internal token
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "a valid delegated JWT must authenticate")
	assert.Equal(t, actingUser, got.UserID, "UserID must come from the delegated acting_user_id")
	assert.Equal(t, ws, got.WorkspaceID, "WorkspaceID must come from the delegated claims")
	assert.Equal(t, domain.SubjectUser, got.SubjectType, "acting-user principal resolves as user")
	assert.True(t, got.IsServiceCaller, "delegated path is still an internal-service caller")
	assert.False(t, got.IsAdmin, "a delegated session is not a platform admin (§4.4)")
	assert.ElementsMatch(t, []string{string(domain.ActionUse), string(domain.ActionRead)}, got.Actions,
		"capability envelope must come from the delegated session")
}

// Test_UseCase8_AgentDelegatedContextResolvesAgent: when the delegated session
// is for an agent acting on behalf of a user, the AuthState carries both the
// agent id and the acting user id, with SubjectType=agent.
func Test_UseCase8_AgentDelegatedContextResolvesAgent(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	ws := uuid.New()
	agentID := uuid.New()
	actingUser := uuid.New()

	token, _, err := dm.IssueDelegated(context.Background(), authz.DelegatedRequest{
		AgentID:       &agentID,
		ActingUserID:  &actingUser,
		WorkspaceID:   ws,
		Actions:       []domain.Action{domain.ActionUse},
		AuthzRevision: 0,
		Audience:      "mcp-server",
	})
	require.NoError(t, err)

	r, got := probeRecorder(t, tm, internal, dm)

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, agentID, got.AgentID, "AgentID must come from the delegated claims")
	assert.Equal(t, actingUser, got.UserID, "ActingUserID must come from the delegated claims")
	assert.Equal(t, domain.SubjectAgent, got.SubjectType, "agent-on-behalf-of-user resolves as agent")
	assert.False(t, got.IsAdmin)
}

// Test_UseCase8_InvalidDelegatedContextRefused: a token that LOOKS delegated
// (carries a `sid` claim) but has a bad signature is refused — the middleware
// does NOT fall back to the user-JWT path or to admin. This closes the UC8
// privilege-escalation gap (a forged delegated token must not pass) and is
// the harder case than a bare garbage string (which lacks the `sid` shape and
// falls through to the user-JWT validator, also 401).
func Test_UseCase8_InvalidDelegatedContextRefused(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	// Manager A issues a delegated token; manager B (different secret) is
	// wired into the middleware. The token carries a valid `sid` shape but its
	// signature does not verify under B → refused.
	issuer, _, _ := newMMDelegatedManager()
	dm, _, _ := newMMDelegatedManager()

	ws := uuid.New()
	token, _, err := issuer.IssueDelegated(context.Background(), authz.DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	r, _ := probeRecorder(t, tm, internal, dm)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token) // valid shape, wrong signer
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "a delegated-shaped token with a bad signature must be refused, not retried as a user")
}

// Test_UseCase8_RevokedDelegatedContextRefused: a delegated session that was
// revoked (§5.6) must be refused on the very next request — no admin fallback.
func Test_UseCase8_RevokedDelegatedContextRefused(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	dm, sessions, _ := newMMDelegatedManager()

	ws := uuid.New()
	actingUser := uuid.New()
	token, _, err := dm.IssueDelegated(context.Background(), authz.DelegatedRequest{
		ActingUserID:  &actingUser,
		WorkspaceID:   ws,
		Actions:       []domain.Action{domain.ActionUse},
		AuthzRevision: 0,
		Audience:      "mcp-server",
	})
	require.NoError(t, err)

	// Revoke the session — locate its id via the fake store.
	var sessionID uuid.UUID
	sessions.mu.Lock()
	for id := range sessions.sessions {
		sessionID = id
	}
	sessions.mu.Unlock()
	_, err = dm.Revoke(context.Background(), sessionID, ws)
	require.NoError(t, err)

	r, _ := probeRecorder(t, tm, internal, dm)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "revoked delegated context must be refused (§5.6 sync-deny)")
}

// --- Regression: the user-JWT (browser/session) path must still authenticate
// after the delegated-context wiring. A user JWT and a delegated JWT share the
// same HS256 secret, so the middleware routes by claims shape (`sid` present ⇒
// delegated; absent ⇒ user). A user JWT must NOT be misrouted to the delegated
// validator (which would refuse it for lacking a session id). These guard the
// non-internal caller path against the §4.4 change.

// Test_Regression_UserJWTViaMiddleware: a standard user JWT authenticates and
// resolves the caller identity (incl. admin flag), unaffected by the
// DelegatedManager being wired.
func Test_Regression_UserJWTViaMiddleware(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", time.Minute)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	uid := uuid.New()
	jwtStr, err := tm.Issue(uid, "user@example.com", "User", nil, false)
	require.NoError(t, err)

	r, got := probeRecorder(t, tm, internal, dm)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "user JWT must still authenticate through the middleware")
	assert.Equal(t, uid, got.UserID)
	assert.Equal(t, "user@example.com", got.Email)
	assert.Equal(t, domain.SubjectUser, got.SubjectType)
	assert.False(t, got.IsServiceCaller, "a user JWT is not an internal service caller")
}

// Test_Regression_AdminUserJWTViaMiddleware: an admin user JWT retains its
// admin flag through the middleware. The delegated-context change must not
// strip admin from a legitimately admin user session.
func Test_Regression_AdminUserJWTViaMiddleware(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", time.Minute)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	uid := uuid.New()
	jwtStr, err := tm.Issue(uid, "root@example.com", "Root", nil, true)
	require.NoError(t, err)

	r, got := probeRecorder(t, tm, internal, dm)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, got.IsAdmin, "admin user JWT must retain IsAdmin")
	assert.Equal(t, uid, got.UserID)
}

// Test_Regression_MissingTokenRefused: a request with no Authorization header
// is refused (the delegated-context wiring must not open an unauthenticated
// path).
func Test_Regression_MissingTokenRefused(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", 0)
	internal := "internal-service-token"
	dm, _, _ := newMMDelegatedManager()

	r, _ := probeRecorder(t, tm, internal, dm)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "missing token must be refused")
}
