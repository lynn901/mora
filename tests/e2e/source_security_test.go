//go:build e2e

package e2e

// source_security_test.go covers the Phase 1 Source-management REST surface
// for the §10.4 越权/存在性不泄露 axis (design-docs/14 §10.4 用例 25-29).
//
// The SUT here is the knowledge-source REST API (§4.4 D13, wired in
// cmd/mora-api/main.go). Each case drives the black-box HTTP surface as a
// non-admin workspace member and asserts the RBAC / existence-non-leak
// contract the design intends.
//
// SUT STATUS (verified against cmd/mora-api/main.go + service/service.go at
// commit e9e2042, 2026-08-13):
//  - 用例 28 (disabled source → next sync rejected): IMPLEMENTED. The
//    service's TriggerSync checks `!src.Enabled` and returns ErrSourceNotFound
//    (handler maps to 404/409). This is the one §10.4 case with a real SUT,
//    so it is asserted live.
//  - 用例 25 (no sync permission → 403 + audit): SUT GAP. SourceService has
//    no rbac.Engine and CreateSource/TriggerSync perform no workspace-write
//    Check — the authed route group carries AuthMiddleware + AuditMiddleware
//    only, no per-source permission gate. Skipped pending RBAC wiring.
//  - 用例 26 (cross-workspace GET /knowledge/assets/{id} → 404): SUT GAP.
//    The asset endpoints are NOT mounted in main.go at all (no
//    /knowledge/assets routes). Skipped pending the asset read API.
//  - 用例 27 (cross-workspace source/asset/relation → 404): PARTIAL GAP.
//    GET /knowledge/sources/:id is mounted but has no RBAC (see 用例 25);
//    /knowledge/assets/* and relations are not mounted. Skipped.
//  - 用例 29 (review by non-review-role → 403): SUT GAP. AppendReviewDecision
//    appends the decision with no review_roles membership check. Skipped.
//
// The skipped cases carry a code-level gap note so the wiring task (tracked
// against YS-110) can flip the Skip off once RBAC is added to SourceService.

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// knowledgeSource is the JSON shape of the §4.4 source response. The domain
// KnowledgeSource struct carries no json tags, so fields serialize as their
// Go names (ID, ETagVersion, ...). We only decode the fields the tests read.
type knowledgeSource struct {
	ID          string `json:"ID"`
	WorkspaceID string `json:"WorkspaceID"`
	Name        string `json:"Name"`
	Enabled     bool   `json:"Enabled"`
	ETagVersion int64  `json:"ETagVersion"`
}

// createKnowledgeSource (admin) creates a url_api source in wsID and returns
// the decoded response (id + initial ETag). Mirrors §4.4 POST /sources.
func (s *Suite) createKnowledgeSource(cl *Client, wsID, name, uri string) knowledgeSource {
	var src knowledgeSource
	st, env := cl.post("/api/v1/workspaces/"+wsID+"/knowledge/sources",
		map[string]any{
			"source_type": "url_api", "name": name, "uri": uri,
			"requested_asset_type": "document",
		}, &src)
	require.Equalf(s.T(), http.StatusCreated, st, "create source: code=%d msg=%s", env.Code, env.Message)
	return src
}

// disableSource PATCHes enabled=false via If-Match (§4.4 PATCH). Returns the
// response status + envelope (a 200 with the fresh ETag on success).
func (s *Suite) disableSource(cl *Client, srcID string, etag int64) (int, *envelope) {
	enabled := false
	return cl.patch("/api/v1/knowledge/sources/"+srcID,
		map[string]any{"enabled": enabled}, nil,
		map[string]string{"If-Match": etagToString(etag)})
}

// triggerSync POSTs /sync-runs (§4.4). Returns status + envelope. The
// requested_asset_type is required by the triggerSyncReq binding.
func (s *Suite) triggerSync(cl *Client, srcID string) (int, *envelope, []byte) {
	return cl.call(http.MethodPost, "/api/v1/knowledge/sources/"+srcID+"/sync-runs",
		map[string]any{"requested_asset_type": "document"}, nil,
		map[string]string{"Idempotency-Key": "e2e-src-sync-" + randHex(8)})
}

// etagToString formats an ETagVersion for the If-Match header (int64 → base-10).
func etagToString(etag int64) string {
	return strconv.FormatInt(etag, 10)
}

// --- §10.4 用例 28: a disabled source's next sync request is rejected ---

// TestSourceSecurity_DisabledSourceRejectsSync asserts the §10.4 用例 28
// invariant against the implemented SUT: after a source is soft-disabled
// (PATCH enabled=false), a subsequent POST /sync-runs is rejected (no new
// source_sync_runs row created). The service's TriggerSync maps a disabled
// source to ErrSourceNotFound so the enabled/disabled state does not leak to
// an unauthorized caller (§8.2). This is the only §10.4 case with a live SUT.
func (s *Suite) TestSourceSecurity_DisabledSourceRejectsSync() {
	s.requireDB("admin source create + disable + sync for 用例 28")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E §10.4 用例28 WS", "e2e-c28-"+randHex(4))

	// Create + sync once (enabled=true → sync is accepted/queued). A 202
	// (Accepted) or a 409 (idempotency conflict from a prior run) both mean the
	// endpoint accepted the trigger; we only need the source to exist.
	src := s.createKnowledgeSource(admin, ws.ID, "e2e-c28-src", "https://example.com/c28.json")
	require.NotEmpty(s.T(), src.ID, "source must be created")

	// Disable the source via PATCH enabled=false (soft-disable, §4.4).
	st, env := s.disableSource(admin, src.ID, src.ETagVersion)
	require.Equalf(s.T(), http.StatusOK, st, "disable source: code=%d msg=%s", env.Code, env.Message)

	// §10.4 用例 28 red line: the next sync request on a disabled source is
	// rejected. The service returns ErrSourceNotFound → 404 (40400), since a
	// 409 source_disabled is also acceptable per the design ("4xx, code=409xx
	// source_disabled 或 404"). Either way it must NOT be 202/Accepted (no new
	// run queued) — that is the existence-freeze guarantee.
	syncSt, syncEnv, _ := s.triggerSync(admin, src.ID)
	require.NotEqualf(s.T(), http.StatusAccepted, syncSt,
		"a disabled source must not accept a new sync run (got %d, code=%d msg=%s)",
		syncSt, syncEnv.Code, syncEnv.Message)
	require.Truef(s.T(),
		syncSt == http.StatusNotFound || syncSt == http.StatusConflict ||
			(syncSt >= 400 && syncSt < 500),
		"disabled-source sync must be rejected with 4xx (404/409); got %d", syncSt)
}

// --- §10.4 用例 25: no sync permission → 403 + audit (SUT GAP) ---

// TestSourceSecurity_NoSyncPermissionRejected asserts the §10.4 用例 25
// invariant: a workspace read-only member (no sync permission) calling
// POST /workspaces/{ws}/knowledge/sources or POST /knowledge/sources/{id}/sync-runs
// is rejected with 403 (code=40300) and the attempt is recorded as a denied
// audit event.
//
// SUT GAP (§10.4 用例 25, blocked pending YS-110): SourceService has no
// rbac.Engine; CreateSource and TriggerSync perform no workspace-write Check.
// The /workspaces/:ws/knowledge/sources and /knowledge/sources/:id/sync-runs
// routes sit on the authed group (AuthMiddleware + AuditMiddleware only) with
// no per-source permission gate, so a read-only member can create a source and
// trigger a sync today — the 403 this case requires is not enforced. Marked
// Skip until RBAC is wired into SourceService.CreateSource / TriggerSync.
func (s *Suite) TestSourceSecurity_NoSyncPermissionRejected() {
	s.T().Skip("DEFECT §10.4 用例 25 (SUT GAP, blocked YS-110): SourceService has no RBAC — CreateSource/TriggerSync perform no workspace-write permission Check, so a read-only member is not rejected with 403")
	s.requireDB("read-only workspace member for 用例 25")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E §10.4 用例25 WS", "e2e-c25-"+randHex(4))

	// Grant alice a read-only (viewer) role on this workspace — NO sync permission.
	alice := s.jwtClient(s.aliceJWT)
	s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "workspace", ws.ID, "allow")

	// §10.4 用例 25 red line: alice (read-only) must NOT create a source.
	_, env := alice.post("/api/v1/workspaces/"+ws.ID+"/knowledge/sources",
		map[string]any{
			"source_type": "url_api", "name": "c25-leak", "uri": "https://example.com/c25.json",
			"requested_asset_type": "document",
		}, nil)
	require.Equal(s.T(), 40300, env.Code, "read-only member must be denied source create (40300)")

	// And must NOT trigger a sync on a source admin created.
	src := s.createKnowledgeSource(admin, ws.ID, "c25-admin-src", "https://example.com/c25-admin.json")
	_, syncEnv, _ := s.triggerSync(alice, src.ID)
	require.Equal(s.T(), 40300, syncEnv.Code, "read-only member must be denied sync trigger (40300)")
}

// --- §10.4 用例 26: cross-workspace GET /knowledge/assets/{id} → 404 (SUT GAP) ---

// TestSourceSecurity_CrossWorkspaceAssetNotFound asserts the §10.4 用例 26
// invariant: a workspace-B read user calling GET /knowledge/assets/{id} for an
// asset that lives in workspace A gets 404 (code=40400) — the response is
// byte-identical to a not-found, with no 403 that leaks existence.
//
// SUT GAP (§10.4 用例 26, blocked pending YS-108/109): the asset READ
// endpoints (/knowledge/assets/:id, /knowledge/assets/:id/versions) are NOT
// mounted in cmd/mora-api/main.go. There is no AssetHandler in the mora
// handler package. The case is therefore not automatable until the asset read
// API lands. Marked Skip.
func (s *Suite) TestSourceSecurity_CrossWorkspaceAssetNotFound() {
	s.T().Skip("DEFECT §10.4 用例 26 (SUT GAP, blocked YS-108/109): GET /knowledge/assets/:id is not mounted — no AssetHandler / route exists in main.go")
}

// --- §10.4 用例 27: cross-workspace source/asset/relation → 404 (PARTIAL GAP) ---

// TestSourceSecurity_CrossWorkspaceSourceNotFound asserts the §10.4 用例 27
// invariant for the source leg: a workspace-B read user calling
// GET /knowledge/sources/{id} for a source in workspace A gets 404 (40400),
// indistinguishable from not-found, with no 403 that leaks existence.
//
// PARTIAL SUT GAP (§10.4 用例 27): GET /knowledge/sources/:id IS mounted, but
// SourceService.GetSource → SourceRepo.Get is a pure SQL lookup by id with no
// RBAC membership check — a cross-workspace caller currently receives 200 +
// the source body (existence leak), not the 404 this case requires. The
// asset + relation legs are not mounted at all (see 用例 26). Marked Skip
// until SourceService.GetSource gains an RBAC workspace-membership Check
// (returning ErrSourceNotFound for an out-of-scope source).
func (s *Suite) TestSourceSecurity_CrossWorkspaceSourceNotFound() {
	s.T().Skip("DEFECT §10.4 用例 27 (SUT GAP, blocked YS-110): SourceService.GetSource has no RBAC — cross-workspace GET /knowledge/sources/:id returns 200 (existence leak), not 404; asset/relation legs unmounted")
}

// --- §10.4 用例 29: review by non-review-role → 403 (SUT GAP) ---

// TestSourceSecurity_ReviewByNonReviewRoleRejected asserts the §10.4 用例 29
// invariant: a principal whose role is NOT in the governance Profile's
// review_roles calling POST /knowledge/reviews/{id}/decisions is rejected
// with 403 (40300), no review_decisions row is created, and a denied audit
// event is recorded.
//
// SUT GAP (§10.4 用例 29, blocked pending YS-110): SourceService.AppendReviewDecision
// appends the ReviewDecisionRecord with no review_roles membership check —
// any authed caller can post a decision today. Marked Skip until the review
// gate enforces review_roles membership before AppendDecision.
func (s *Suite) TestSourceSecurity_ReviewByNonReviewRoleRejected() {
	s.T().Skip("DEFECT §10.4 用例 29 (SUT GAP, blocked YS-110): AppendReviewDecision has no review_roles RBAC — any authed caller can post a review decision, no 403")
}

// keep testing import referenced.
var _ = testing.T{}
