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
// commit e9e2042, 2026-08-13, rebased onto PR #45 SourceService.WithAuthz):
//  - 用例 28 (disabled source → next sync rejected): IMPLEMENTED. TriggerSync
//    checks `!src.Enabled` and returns ErrSourceNotFound → 404/409. Asserted live.
//  - 用例 25 (no sync permission → 403): IMPLEMENTED. WithAuthz authorizes
//    CreateSource (ActionWrite) + TriggerSync (ActionSync) with leak=true →
//    ErrSourceForbidden → 403/40300 for a viewer. Asserted live.
//  - 用例 27 (cross-workspace source GET → 404): IMPLEMENTED. GetSource
//    authorizes ActionRead (leak=false); a cross-workspace source resolves
//    as not-found → ErrSourceNotFound → 404/40400, byte-identical to
//    not-found. Source leg asserted live; asset/relation legs still
//    unmounted (see 用例 26).
//  - 用例 29 (review by non-review-role → 403): IMPLEMENTED. AppendReviewDecision
//    authorizes ActionReview (leak=true) → ErrSourceForbidden → 403/40300 for
//    a caller whose role lacks ActionReview. Asserted live (seeded review chain).
//  - 用例 26 (cross-workspace GET /knowledge/assets/{id} → 404): SUT GAP.
//    The asset endpoints are NOT mounted in main.go (no /knowledge/assets
//    routes). Skipped pending the asset read API.

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
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

// --- §10.4 用例 25: no sync permission → 403 + audit ---

// TestSourceSecurity_NoSyncPermissionRejected asserts the §10.4 用例 25
// invariant: a workspace read-only member (viewer = ["read"], no write/sync)
// calling POST /workspaces/{ws}/knowledge/sources or
// POST /knowledge/sources/{id}/sync-runs is rejected with 403 (code=40300).
//
// SourceService.WithAuthz (PR #45, YS-115) now authorizes CreateSource with
// ActionWrite (leak=true → ErrSourceForbidden) and TriggerSync with
// ActionSync (leak=true → ErrSourceForbidden); the handler's mapSourceErr
// routes ErrSourceForbidden to 403 + 40300. The viewer role grants only
// ActionRead, so both writes deny. This is the e2e (black-box HTTP) leg of
// the contract the backend pins at service/service_authz_test.go.
func (s *Suite) TestSourceSecurity_NoSyncPermissionRejected() {
	s.requireDB("read-only workspace member for 用例 25")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E §10.4 用例25 WS", "e2e-c25-"+randHex(4))

	// Grant alice a read-only (viewer) role on this workspace — NO sync permission.
	alice := s.jwtClient(s.aliceJWT)
	s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "workspace", ws.ID, "allow")

	// §10.4 用例 25 red line: alice (read-only) must NOT create a source.
	createSt, createEnv := alice.post("/api/v1/workspaces/"+ws.ID+"/knowledge/sources",
		map[string]any{
			"source_type": "url_api", "name": "c25-leak", "uri": "https://example.com/c25.json",
			"requested_asset_type": "document",
		}, nil)
	require.Equalf(s.T(), http.StatusForbidden, createSt,
		"read-only member must be denied source create (403); got %d", createSt)
	require.Equal(s.T(), 40300, createEnv.Code, "read-only member must be denied source create (40300)")

	// And must NOT trigger a sync on a source admin created.
	src := s.createKnowledgeSource(admin, ws.ID, "c25-admin-src", "https://example.com/c25-admin.json")
	syncSt, syncEnv, _ := s.triggerSync(alice, src.ID)
	require.Equalf(s.T(), http.StatusForbidden, syncSt,
		"read-only member must be denied sync trigger (403); got %d", syncSt)
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

// --- §10.4 用例 27: cross-workspace source → 404 (no existence leak) ---

// TestSourceSecurity_CrossWorkspaceSourceNotFound asserts the §10.4 用例 27
// invariant for the source leg: a workspace-B read user calling
// GET /knowledge/sources/{id} for a source that lives in workspace A gets 404
// (code=40400), byte-identical to a not-found — no 403 that leaks existence.
//
// SourceService.WithAuthz (PR #45, YS-115) authorizes GetSource with
// ActionRead (leak=false): the CompositeLocator resolves a cross-workspace
// source as not-resolvable → ErrSourceNotFound → 404. The asset + relation
// legs of 用例 27 are still unmounted (see 用例 26), so this case covers the
// source leg that IS wired. This is the e2e leg of the contract the backend
// pins at service/service_authz_test.go::TestAuthz_GetSource_CrossWorkspaceIsNotFound.
func (s *Suite) TestSourceSecurity_CrossWorkspaceSourceNotFound() {
	s.requireDB("cross-workspace read caller for 用例 27")
	admin := s.adminClient()
	// Workspace A: admin creates a source here.
	wsA := s.createWorkspace(admin, "E2E §10.4 用例27 WS-A", "e2e-c27a-"+randHex(4))
	src := s.createKnowledgeSource(admin, wsA.ID, "c27-src-a", "https://example.com/c27a.json")
	require.NotEmpty(s.T(), src.ID, "source in wsA must exist")

	// Workspace B: bob is a read-only member. He has NO grant on wsA.
	wsB := s.createWorkspace(admin, "E2E §10.4 用例27 WS-B", "e2e-c27b-"+randHex(4))
	bob := s.jwtClient(s.bobJWT)
	s.grantPermission(admin, "user", s.bobUserID, s.viewerRoleID, "workspace", wsB.ID, "allow")

	// §10.4 用例 27 red line: bob (wsB member) GETs wsA's source → 404, not 200/403.
	var out knowledgeSource
	st, env := bob.get("/api/v1/knowledge/sources/"+src.ID, &out)
	require.Equalf(s.T(), http.StatusNotFound, st,
		"cross-workspace source GET must be 404 (no existence leak); got %d", st)
	require.Equal(s.T(), 40400, env.Code, "cross-workspace source must return 40400 (not 40300, which would leak existence)")
	require.Empty(s.T(), out.ID, "no source body must be returned for a cross-workspace source")

	// Control: a source that genuinely does not exist must return the SAME
	// 404/40400 — bob cannot distinguish cross-workspace from not-found.
	st2, env2 := bob.get("/api/v1/knowledge/sources/00000000-0000-0000-0000-000000000000", &out)
	require.Equal(s.T(), http.StatusNotFound, st2, "non-existent source must be 404")
	require.Equal(s.T(), 40400, env2.Code, "non-existent source must be 40400, same as cross-workspace")
}

// --- §10.4 用例 29: review by non-review-role → 403 ---

// seedReviewRequest inserts the minimal FK chain a review_requests row needs
// (governance_profile + knowledge_asset + knowledge_asset_version + the
// review_request itself) inside one transaction. It mirrors the shape the
// service's own CreateRequest writes. Deleting the workspace CASCADES to all
// four, so the e2e workspace-cleanup covers this seed.
//
// The governance_profile is created with review_roles=[] (no role is a
// reviewer) — mirroring the 014 migration's legacy_migration system profile.
// That makes the RBAC ActionReview grant the deciding factor: a caller whose
// role grants ActionReview may review; a viewer (only ActionRead) may not.
// The system roles seeded by 005_rbac grant read/write/admin/sync — none grant
// "review" — so a non-admin viewer is always denied ActionReview → 403.
func (s *Suite) seedReviewRequest(wsIDStr, userIDStr string) uuid.UUID {
	ctx := context.Background()
	wsID, err := uuid.Parse(wsIDStr)
	require.NoError(s.T(), err, "parse workspace id")
	userID, err := uuid.Parse(userIDStr)
	require.NoError(s.T(), err, "parse user id")
	tx, err := s.pool.Begin(ctx)
	require.NoError(s.T(), err, "begin review-seed tx")
	defer tx.Rollback(ctx) // safe: Commit commits the committed state.

	// governance_profile (workspace-scoped; review_roles=[]).
	var govID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO governance_profiles
		  (workspace_id, name, asset_type, transition_rules, review_roles,
		   auto_publish, required_projections, is_system)
		VALUES ($1,'e2e-c29-gov','document','{}'::jsonb,'[]'::jsonb,
		        '{}'::jsonb,'["fts"]'::jsonb,false)
		RETURNING id`, wsID).Scan(&govID)
	require.NoError(s.T(), err, "seed governance_profile")

	// knowledge_asset (document, published, points at gov profile).
	var assetID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO knowledge_assets
		  (workspace_id, asset_type, name, owner_type, owner_id, status,
		   governance_profile_id)
		VALUES ($1,'document','e2e-c29-asset','user',$2,'published',$3)
		RETURNING id`, wsID, userID, govID).Scan(&assetID)
	require.NoError(s.T(), err, "seed knowledge_asset")

	// knowledge_asset_version (ready+published so it is the usable current).
	var versionID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, content_origin, dedupe_key,
		   build_status, governance_status, created_by_type, created_by_id)
		VALUES ($1,1,'human',$2,'ready','published','user',$3)
		RETURNING id`, assetID, "seed:"+assetID.String(), userID).Scan(&versionID)
	require.NoError(s.T(), err, "seed knowledge_asset_version")
	_, err = tx.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$2 WHERE id=$1`, assetID, versionID)
	require.NoError(s.T(), err, "link current_version_id")

	// review_request (pending) referencing asset + version + gov profile.
	var reviewID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO review_requests
		  (workspace_id, asset_id, asset_version_id, governance_profile_id,
		   requested_by_type, requested_by_id, status, rationale)
		VALUES ($1,$2,$3,$4,'user',$5,'pending','e2e-c29 pending review')
		RETURNING id`, wsID, assetID, versionID, govID, userID).Scan(&reviewID)
	require.NoError(s.T(), err, "seed review_request")

	require.NoError(s.T(), tx.Commit(ctx), "commit review-seed tx")
	return reviewID
}

// postDecision POSTs /knowledge/reviews/{id}/decisions (§4.2 governance). The
// handler binds decision + policy_version (required). Returns status + envelope.
func (s *Suite) postDecision(cl *Client, reviewID uuid.UUID) (int, *envelope) {
	return cl.post("/api/v1/knowledge/reviews/"+reviewID.String()+"/decisions",
		map[string]any{
			"decision":       "approve",
			"policy_version": "e2e-c29-v1",
			"rationale":      "e2e approve",
		}, nil)
}

// reviewDecisionCount reads back review_decisions for a request (audit trail
// invariant: a denied caller must not append a decision row).
func (s *Suite) reviewDecisionCount(reviewID uuid.UUID) int {
	var n int
	err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM review_decisions WHERE review_request_id=$1`, reviewID).Scan(&n)
	require.NoError(s.T(), err, "count review_decisions")
	return n
}

// TestSourceSecurity_ReviewByNonReviewRoleRejected asserts the §10.4 用例 29
// invariant against the WithAuthz SUT: a caller whose role does NOT grant
// ActionReview (a workspace viewer — only ActionRead) calling
// POST /knowledge/reviews/{id}/decisions is rejected with 403 (40300), and no
// review_decisions row is appended.
//
// WithAuthz (PR #45, YS-115) authorizes AppendReviewDecision with ActionReview
// (leak=true): the CompositeLocator resolves a workspace-local review; the
// viewer role (005_rbac: ["read"]) holds no "review" grant → default deny →
// ErrSourceForbidden → 403/40300. The positive control (admin, who bypasses
// RBAC via auth.IsAdmin) appending a decision on the SAME review → 204 proves
// the review genuinely exists and is reviewable, so bob's 403 is a real
// permission denial, not a not-found masked as 403.
func (s *Suite) TestSourceSecurity_ReviewByNonReviewRoleRejected() {
	s.requireDB("seeded review chain for 用例 29")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E §10.4 用例29 WS", "e2e-c29-"+randHex(4))

	// Bob is a read-only (viewer) member of this workspace — NO ActionReview.
	bob := s.jwtClient(s.bobJWT)
	s.grantPermission(admin, "user", s.bobUserID, s.viewerRoleID, "workspace", ws.ID, "allow")

	// Seed a pending review_request in this workspace (admin as requester).
	reviewID := s.seedReviewRequest(ws.ID, s.adminUserID())

	// §10.4 用例 29 red line: bob (viewer, not a reviewer) must be denied 403.
	st, env := s.postDecision(bob, reviewID)
	require.Equalf(s.T(), http.StatusForbidden, st,
		"a non-reviewer must be denied review decision (403); got %d", st)
	require.Equal(s.T(), 40300, env.Code, "a non-reviewer must get 40300 (not 40400, which would leak existence)")
	// No decision row was appended by the denied caller.
	require.Equal(s.T(), 0, s.reviewDecisionCount(reviewID),
		"a denied review caller must not append a review_decisions row")

	// Positive control: admin (IsAdmin bypass) posts a decision on the SAME
	// review → 204, proving the review exists + is reviewable, so bob's 403 is
	// a permission denial rather than a masked not-found.
	st2, env2 := s.postDecision(admin, reviewID)
	require.Equalf(s.T(), http.StatusNoContent, st2,
		"admin must be able to post a review decision (positive control); got %d code=%d msg=%s",
		st2, env2.Code, env2.Message)
	require.Equal(s.T(), 1, s.reviewDecisionCount(reviewID),
		"admin's decision must append exactly one review_decisions row")
}

// keep testing import referenced.
var _ = testing.T{}
