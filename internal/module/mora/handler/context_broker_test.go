package handler

// context_broker_test.go pins the §11 REST/internal handler contract: the
// three-state → §11.5 envelope mapping (leak-safe: authorized-empty + no-
// results → 200 empty; provider-down → 503), the §7.2 PUT validation
// (exclude_when ∩ must_surface_conflicts = ∅ rejected with 400), and the
// request→KnowledgeQuery binding. The handler is a thin HTTP adapter; the
// ten-step pipeline is covered by context/broker_test.go. Here we verify the
// HTTP seam, including an end-to-end Context call over a real Broker built on
// fake ports (so the §11.5 status mapping is exercised through the stack).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	knowledgectx "github.com/lynn901/mora/internal/module/knowledge/context"
	"github.com/lynn901/mora/internal/platform/authz"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
)

// ---------------------------------------------------------------------------
// Pure mapping: toContextResponse carries the three-state fields the handler
// relies on (intent, policy_version, authz_revision, decision_id, degraded,
// truncation). §11.3 wire shape.
// ---------------------------------------------------------------------------

func TestToContextResponse_CarriesThreeStateFields(t *testing.T) {
	dec := uuid.New()
	docID := uuid.New()
	r := knowledgectx.ContextResponse{
		Candidates: []knowledgectx.KnowledgeCandidate{{
			AssetID:   docID,
			AssetType: domain.AssetTypeDocument,
			Title:     "doc",
			Citation:  knowledgectx.Citation{AssetID: docID, AssetType: domain.AssetTypeDocument},
		}},
		DegradedSources: []knowledgectx.DegradedSource{
			{AssetType: domain.AssetTypeCodebase, Reason: knowledgectx.DegradedProviderTimeout, Detail: "codegraph timeout"},
		},
		Truncation: knowledgectx.TruncationReport{
			Reason:            "budget_exceeded",
			TruncatedAssetIDs: []string{uuid.New().String()},
			ContinueTools:     []string{"document_read"},
		},
		Intent:        knowledgectx.IntentSpec,
		PolicyVersion: 3,
		AuthzRevision: 99,
		DecisionID:    &dec,
	}
	out := toContextResponse(r)
	require.Equal(t, "spec", out.Intent)
	require.Equal(t, 3, out.PolicyVersion)
	require.Equal(t, int64(99), out.AuthzRevision)
	require.Equal(t, dec.String(), out.DecisionID)
	require.Len(t, out.Candidates, 1)
	require.Equal(t, docID.String(), out.Candidates[0].AssetID)
	require.Len(t, out.DegradedSources, 1)
	require.Equal(t, "codebase", out.DegradedSources[0].AssetType)
	require.Equal(t, "provider_timeout", out.DegradedSources[0].Reason)
	require.Equal(t, "budget_exceeded", out.Truncation.Reason)
	require.Equal(t, []string{"document_read"}, out.Truncation.ContinueTools)
}

// ---------------------------------------------------------------------------
// §11.5 leak-safe mapping: authorized-empty + no-results → 200 (NOT 403/404).
// Only IsProviderDown → 503. Verified end-to-end through the handler over a real
// Broker built on fake ports.
// ---------------------------------------------------------------------------

// handlerFakes holds the fake ports a handler test wires under a real Broker.
type handlerFakes struct {
	mem    *fakeHandlerMemoryPort
	doc    *fakeHandlerDocumentPort
	auth   *fakeHandlerAuthz
	broker *knowledgectx.Broker
}

// newHandlerBroker builds a real Broker over fake ports + a fake authz that
// allows everything. The handler under test is constructed over this Broker.
func newHandlerBroker(t *testing.T, memOut []knowledgectx.KnowledgeCandidate, memErr error, docOut []knowledgectx.KnowledgeCandidate, docErr error) *handlerFakes {
	t.Helper()
	auth := &fakeHandlerAuthz{authorizeAllowed: true}
	mem := &fakeHandlerMemoryPort{out: memOut, err: memErr}
	doc := &fakeHandlerDocumentPort{out: docOut, err: docErr}
	b := knowledgectx.NewBroker(
		mem,
		knowledgectx.NewIntentRouter(nil),
		auth,
		nil,
		knowledgectx.NewBudgeter(),
		knowledgectx.NewCitationBuilder(nil),
		knowledgectx.DefaultBudgetDefaults,
		knowledgectx.WithDocumentPort(doc),
	)
	return &handlerFakes{mem: mem, doc: doc, auth: auth, broker: b}
}

type fakeHandlerAuthz struct {
	authorizeAllowed bool
}

func (f *fakeHandlerAuthz) Authorize(ctx context.Context, req authz.AuthzRequest) (authz.AuthzContext, error) {
	return authz.AuthzContext{
		WorkspaceID:   req.WorkspaceID,
		AuthzRevision: 1,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		AgentID:       req.AgentID,
		Allowed:       f.authorizeAllowed,
	}, nil
}

func (f *fakeHandlerAuthz) VisibleAssets(ctx context.Context, scope authz.ListScope, candidates []uuid.UUID) ([]uuid.UUID, error) {
	return candidates, nil // all visible
}

type fakeHandlerMemoryPort struct {
	out []knowledgectx.KnowledgeCandidate
	err error
}

func (f *fakeHandlerMemoryPort) Recall(ctx context.Context, ac knowledgectx.AuthzContext, q knowledgectx.MemoryQueryRequest) ([]knowledgectx.KnowledgeCandidate, error) {
	return f.out, f.err
}

type fakeHandlerDocumentPort struct {
	out []knowledgectx.KnowledgeCandidate
	err error
}

func (f *fakeHandlerDocumentPort) Search(ctx context.Context, ac knowledgectx.AuthzContext, q knowledgectx.DocumentQueryRequest) ([]knowledgectx.KnowledgeCandidate, error) {
	return f.out, f.err
}

// dispatchContext builds the handler + router + request + recorder for a
// POST /knowledge/context call, authenticated as a user principal.
func dispatchContext(t *testing.T, b *knowledgectx.Broker, wsID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewContextBrokerHandler(b, nil)
	st := AuthState{UserID: uuid.New(), SubjectType: domain.SubjectUser}
	r := withAuth(st, http.MethodPost, "/workspaces/:workspace_id/knowledge/context", h.Context)
	path := "/workspaces/" + wsID.String() + "/knowledge/context"
	req := httptest.NewRequest(http.MethodPost, path, strReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// §11.5: HasResults → 200 with candidates
// ---------------------------------------------------------------------------

func TestContextHandler_HasResults_200(t *testing.T) {
	ws := uuid.New()
	docID := uuid.New()
	f := newHandlerBroker(t,
		nil, nil,
		[]knowledgectx.KnowledgeCandidate{{AssetID: docID, AssetType: domain.AssetTypeDocument, Title: "doc", Score: 0.9, Authority: 0.9, Freshness: 0.8, Citation: knowledgectx.Citation{AssetID: docID, AssetType: domain.AssetTypeDocument}}},
		nil,
	)
	w := dispatchContext(t, f.broker, ws, `{"query":"spec","asset_types":["document"]}`)
	require.Equal(t, http.StatusOK, w.Code)
	// The response envelope is {code,data}; data is the contextResponse shape.
	// Decode loosely into a map to inspect candidates without coupling to the
	// handler's wire struct.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	data, _ := raw["data"].(map[string]any)
	cands, _ := data["candidates"].([]any)
	require.Len(t, cands, 1)
	require.Equal(t, "spec", data["intent"])
}

// ---------------------------------------------------------------------------
// §11.5: IsEmptyNoResults → 200 with empty candidate list (NOT 404). Leak-safe.
// ---------------------------------------------------------------------------

func TestContextHandler_NoResults_200Empty(t *testing.T) {
	ws := uuid.New()
	f := newHandlerBroker(t, nil, nil, nil, nil)
	w := dispatchContext(t, f.broker, ws, `{"query":"why","asset_types":["document"]}`)
	require.Equal(t, http.StatusOK, w.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	data, _ := raw["data"].(map[string]any)
	cands, _ := data["candidates"].([]any)
	require.Empty(t, cands)
}

// ---------------------------------------------------------------------------
// §11.5: IsProviderDown → 503 (a fault, never "no knowledge"). Verified
// end-to-end: all ports fail.
// ---------------------------------------------------------------------------

func TestContextHandler_ProviderDown_503(t *testing.T) {
	ws := uuid.New()
	f := newHandlerBroker(t,
		nil, errFake("memory engine down"),
		nil, errFake("qdrant down"),
	)
	w := dispatchContext(t, f.broker, ws, `{"query":"spec","asset_types":["document","memory"]}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// errFake is a simple error for the fake ports.
type errFake string

func (e errFake) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// §11.5: no principal → 401 (leak-safe: NOT 403, which would imply the
// resource exists).
// ---------------------------------------------------------------------------

func TestContextHandler_NoPrincipal_401(t *testing.T) {
	ws := uuid.New()
	f := newHandlerBroker(t, nil, nil, nil, nil)
	h := NewContextBrokerHandler(f.broker, nil)
	// No UserID, no AgentID → authStatePrincipal returns Unauthorized.
	r := withAuth(AuthState{}, http.MethodPost, "/workspaces/:workspace_id/knowledge/context", h.Context)
	req := httptest.NewRequest(http.MethodPost, "/workspaces/"+ws.String()+"/knowledge/context", strReader(`{"query":"spec"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---------------------------------------------------------------------------
// §11.5: no workspace_id → 400
// ---------------------------------------------------------------------------

func TestContextHandler_NoWorkspace_400(t *testing.T) {
	f := newHandlerBroker(t, nil, nil, nil, nil)
	h := NewContextBrokerHandler(f.broker, nil)
	st := AuthState{UserID: uuid.New(), SubjectType: domain.SubjectUser}
	r := withAuth(st, http.MethodPost, "/workspaces/:workspace_id/knowledge/context", h.Context)
	// workspace_id path param is not a uuid → falls to body; body has none → 400.
	req := httptest.NewRequest(http.MethodPost, "/workspaces/not-a-uuid/knowledge/context", strReader(`{"query":"spec"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---------------------------------------------------------------------------
// §7.2 PUT validation: exclude_when ∩ must_surface_conflicts ≠ ∅ → 400
// ---------------------------------------------------------------------------

func TestUpdatePolicy_CollisionRejected_400(t *testing.T) {
	f := newHandlerBroker(t, nil, nil, nil, nil)
	h := NewContextBrokerHandler(f.broker, &fakePolicyRepo{})
	st := AuthState{UserID: uuid.New(), SubjectType: domain.SubjectUser}
	r := withAuth(st, http.MethodPut, "/workspaces/:workspace_id/knowledge/policies/:intent", h.UpdatePolicy)
	ws := uuid.New()
	// "old_spec" in BOTH must_surface_conflicts AND exclude_when → §7.2 collision.
	body := `{"must_surface_conflicts":["old_spec"],"exclude_when":["old_spec"],"weights":{"document":0.9}}`
	req := httptest.NewRequest(http.MethodPut, "/workspaces/"+ws.String()+"/knowledge/policies/spec", strReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---------------------------------------------------------------------------
// §7.2 PUT validation: a clean config (no collision) is accepted → 200
// ---------------------------------------------------------------------------

func TestUpdatePolicy_CleanConfigAccepted_200(t *testing.T) {
	repo := &fakePolicyRepo{}
	f := newHandlerBroker(t, nil, nil, nil, nil)
	h := NewContextBrokerHandler(f.broker, repo)
	st := AuthState{UserID: uuid.New(), SubjectType: domain.SubjectUser}
	r := withAuth(st, http.MethodPut, "/workspaces/:workspace_id/knowledge/policies/:intent", h.UpdatePolicy)
	ws := uuid.New()
	body := `{"must_surface_conflicts":["contradicts"],"exclude_when":["deprecated"],"weights":{"document":0.9,"codebase":0.5}}`
	req := httptest.NewRequest(http.MethodPut, "/workspaces/"+ws.String()+"/knowledge/policies/spec", strReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, repo.upsertCalled, "Upsert should be called for a clean config")
}

// ---------------------------------------------------------------------------
// §7.2 PUT validation: unknown intent → 400
// ---------------------------------------------------------------------------

func TestUpdatePolicy_UnknownIntent_400(t *testing.T) {
	f := newHandlerBroker(t, nil, nil, nil, nil)
	h := NewContextBrokerHandler(f.broker, &fakePolicyRepo{})
	st := AuthState{UserID: uuid.New(), SubjectType: domain.SubjectUser}
	r := withAuth(st, http.MethodPut, "/workspaces/:workspace_id/knowledge/policies/:intent", h.UpdatePolicy)
	ws := uuid.New()
	req := httptest.NewRequest(http.MethodPut, "/workspaces/"+ws.String()+"/knowledge/policies/bogus", strReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---------------------------------------------------------------------------
// mapBrokerErr: caller/config errors map to leak-safe statuses (no 403)
// ---------------------------------------------------------------------------

func TestMapBrokerErr_LeakSafe(t *testing.T) {
	require.Equal(t, http.StatusBadRequest, statusOf(mapBrokerErr(knowledgectx.ErrBrokerNoWorkspace)))
	require.Equal(t, http.StatusUnauthorized, statusOf(mapBrokerErr(knowledgectx.ErrBrokerNoPrincipal)))
	// no-policy is a 500 (wiring fault), not a 403.
	require.Equal(t, http.StatusInternalServerError, statusOf(mapBrokerErr(knowledgectx.ErrBrokerNoPolicy)))
}

// statusOf extracts the HTTP status a pkgerr.Error carries. The pkgerr layer
// stores Status as a field (not a method); mapBrokerErr returns *pkgerr.Error
// for the three Broker sentinels, so a direct type assertion reads it. A
// non-pkgerr error defaults to 500 (the response.Fail fallback).
func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var pe *pkgerr.Error
	if errors.As(err, &pe) {
		return pe.Status
	}
	return http.StatusInternalServerError
}

// ---------------------------------------------------------------------------
// parseAssetTypes: unknowns dropped, no leak (§8.2 — a malformed type does not
// surface a partial asset_type list error)
// ---------------------------------------------------------------------------

func TestParseAssetTypes_DropsUnknowns(t *testing.T) {
	out := parseAssetTypes([]string{"document", "bogus", "memory"})
	require.Equal(t, []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory}, out)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fakePolicyRepo is a no-IO PolicyRepo that records the Upsert + returns the
// record with policy_version+1. ListByWorkspace/LoadCurrent report not-found
// (the built-in default path).
type fakePolicyRepo struct {
	upsertCalled bool
	lastIntent   knowledgectx.Intent
}

func (f *fakePolicyRepo) LoadCurrent(ctx context.Context, workspaceID uuid.UUID, intent knowledgectx.Intent) (knowledgectx.AuthorityPolicyRecord, error) {
	return knowledgectx.AuthorityPolicyRecord{}, knowledgectx.ErrPolicyNotFound
}
func (f *fakePolicyRepo) Upsert(ctx context.Context, rec knowledgectx.AuthorityPolicyRecord) (knowledgectx.AuthorityPolicyRecord, error) {
	f.upsertCalled = true
	f.lastIntent = rec.Intent
	rec.PolicyVersion = 1
	rec.IsCurrent = true
	return rec, nil
}
func (f *fakePolicyRepo) ListByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]knowledgectx.AuthorityPolicyRecord, error) {
	return nil, nil
}
func (f *fakePolicyRepo) CurrentVersion(ctx context.Context, workspaceID uuid.UUID, intent knowledgectx.Intent) (int, error) {
	return 0, nil
}

// strReader returns an io.Reader for a string (test helper).
func strReader(s string) *strReaderT {
	return &strReaderT{s: s}
}

type strReaderT struct {
	s   string
	pos int
}

func (r *strReaderT) Read(p []byte) (int, error) {
	if r.pos >= len(r.s) {
		return 0, errEOF
	}
	n := copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}

// errEOF is a sentinel for the strReader end.
type errEOFT string

func (errEOFT) Error() string { return "EOF" }

var errEOF = errEOFT("EOF")

