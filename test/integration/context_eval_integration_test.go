//go:build integration

// context_eval_integration_test.go — YS-227 §9.3 real-PG eval: eval.Run() over
// the production *context.Broker + seeded dataset, gate assertion, threshold
// lock into context_eval_runs, and three-state + authz-leak-0 re-verification.
//
// DATABASE_URL-gated; skipped when unset. Run with:
//
//	DATABASE_URL=postgres://mora:mora@mora-postgres-1:5432/mora?sslmode=disable \
//	  docker run --rm --network mora_default -v "$PWD:/src" -v mora-gomodcache:/go/pkg/mod \
//	  -w /src -e DATABASE_URL="$DATABASE_URL" golang:1.25-alpine \
//	  go test -tags=integration ./test/integration/... -run ContextEvalSuite -count=1 -v
//
// This closes the YS-211 DoD item 7 ("发布前锁定当期数据集阈值"): the offline
// runner scored against fake-broker in runner_test.go (the math); here it scores
// against the REAL production Broker pipeline (authz two-pass + dedup + policy +
// budget + citation) over the seeded 18-fixture dataset on mora-postgres-1. The
// release-threshold row is written to context_eval_runs with DatasetTag=
// phase6-v1 (DoD §4).
package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
	"github.com/lynn901/mora/internal/module/knowledge/context/eval"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// ContextEvalSuite is the YS-227 §9.3 release-gate suite.
type ContextEvalSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB
	ds   *evalDataset
}

func TestContextEvalSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(ContextEvalSuite))
}

func (s *ContextEvalSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	// Seed the 18-fixture dataset (idempotent UPSERTs by fixed UUID).
	s.ds = seedEvalDataset(s.T(), pool)
}

func (s *ContextEvalSuite) TearDownSuite() { s.pool.Close() }

// SetupTest cleans the eval-owned rows so a re-run starts clean, in dependency
// order (children before parents). The seed's UPSERTs re-populate; cleaning
// avoids stale relation/evidence rows from a prior shape. The eval workspace +
// its assets (fixed UUIDs) are preserved by the UPSERT seed; this clean targets
// only the transient policy/eval/relation/evidence rows that might drift.
func (s *ContextEvalSuite) SetupTest() {
	ctx := context.Background()
	for _, t := range []string{
		"context_eval_runs", "context_authority_policies",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t+" WHERE workspace_id = $1", s.ds.wsID)
	}
	// re-seed policies (cleaned above) so policy_version starts at 1.
	polRepo := postgres.NewContextPolicyRepo(s.db)
	for _, intent := range []ctxmod.Intent{
		ctxmod.IntentSpec, ctxmod.IntentRevision, ctxmod.IntentRationale, ctxmod.IntentProcedure,
	} {
		seedEvalPolicy(s.T(), polRepo, s.ds.wsID, intent)
	}
}

// newEvalAuthzService wires the platform/authz.Service over the real repos,
// exactly like ContextBrokerSuite.newAuthzService (the production resolution
// path under test).
func (s *ContextEvalSuite) newEvalAuthzService() *authz.Service {
	perms := postgres.NewPermissionRepo(s.db)
	repo := postgres.NewRBACAdapter(perms, postgres.NewDirectoryRepo(s.db), postgres.NewDocumentRepo(s.db))
	eng := rbac.NewEngine(repo)
	comp := authz.NewCompositeLocator(
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetWorkspace, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetDirectory, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetDocument, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetAsset, Loc: authz.NewAssetLocator(postgres.NewAuthzAssetRepo(s.db))},
	)
	eng.SetLocator(authz.AsLocator(comp))
	return authz.NewService(comp, eng, postgres.NewBindingRepo(s.db), postgres.NewAuthzAgentRepo(s.db),
		postgres.NewAuthzAssetRepo(s.db), postgres.NewAuthzAssetVersionRepo(s.db),
		postgres.NewRevisionsRepo(s.db), postgres.NewDecisionRepo(s.db))
}

// newEvalBroker wires the production *ctxmod.Broker over: real authz.Service,
// real ContextPolicyRepo (DefaultPolicyLoader), real DedupAndBudgeter +
// CitationBuilder (nil-lookup, §8.2 no-IO path — the ports carry the §8.1
// anchor), and retrieval-map typed ports (runner.go note: fake ports that
// surface the dataset's seeded candidates). The production MemoryAdapter is NOT
// wired: it cannot surface the twin case's same-AssetID document candidates,
// and the production code/skill adapters need codegraph/MinIO (not running).
// The BROKER PIPELINE the ports feed — authz two-pass + dedup + policy score +
// budget + citation — is the real production path over real PG; that is what
// §9.3 scores and what the unit tests cannot (the "单测绿 ≠ 集成绿" red line).
//
// The evalBroker wrapper rewrites the case workspace-literal → seeded wsID.
func (s *ContextEvalSuite) newEvalBroker() *evalBroker {
	authzSvc := s.newEvalAuthzService()
	policyRepo := postgres.NewContextPolicyRepo(s.db)

	broker := ctxmod.NewBroker(
		&evalMemPort{ws: s.ds.wsID},
		ctxmod.NewIntentRouter(nil),
		authzSvc,
		ctxmod.DefaultPolicyLoader(policyRepo),
		ctxmod.NewBudgeter(),
		ctxmod.NewCitationBuilder(nil),
		ctxmod.DefaultBudgetDefaults,
		ctxmod.WithDocumentPort(&evalDocPort{ws: s.ds.wsID}),
		ctxmod.WithCodePort(&evalCodePort{ws: s.ds.wsID}),
		ctxmod.WithSkillPort(&evalSkillPort{ws: s.ds.wsID}),
	)
	return &evalBroker{
		inner: broker, ws: s.ds.wsID, owner: s.ds.owner, ds: s.ds, pool: s.pool,
	}
}

// evalPrincipal is the runner's fixed principal (eval.evalPrincipal's UUID),
// resolved to the seeded workspace_admin. SubjectType=user; PrincipalID=owner.
func (s *ContextEvalSuite) evalPrincipal() ctxmod.Principal {
	return ctxmod.Principal{
		SubjectType:  domain.SubjectUser,
		PrincipalID:  s.ds.owner,
	}
}

// ---------------------------------------------------------------------------
// DoD §3 + §4: eval.Run() over the production Broker → real scores per
// (intent×type); CitationAccuracy ≥ 95% gate; lock context_eval_runs.
// ---------------------------------------------------------------------------

// TestEval_RealPG_RunAndLockThresholds runs eval.Run() against the production
// Broker + seeded dataset, asserts the recall/citation/latency gates pass (the
// release threshold), and locks the per-(intent×type) scores into
// context_eval_runs with DatasetTag=phase6-v1.
func (s *ContextEvalSuite) TestEval_RealPG_RunAndLockThresholds() {
	ctx := context.Background()
	broker := s.newEvalBroker()

	summary, err := eval.Run(ctx, broker, eval.DefaultCaseSets, s.evalPrincipal())
	require.NoError(s.T(), err)
	s.T().Logf("\n%s", eval.FormatReport(summary))

	// DoD §3: per-(intent×type) reporting, NOT aggregated. Assert every expected
	// (intent×type) bucket is present (16 base + 2 conflict cases → ≥16 buckets).
	assert.NotEmpty(s.T(), summary.ByType, "expected per-(intent×type) reports")

	// DoD §4: CitationAccuracy ≥ 95% gate. The release threshold.
	assert.True(s.T(), summary.CitationOK,
		"CitationAccuracy gate failed (< 95%%); see report for CitationMisses:\n%s", eval.FormatReport(summary))

	// Recall@K gate (≥ 0.9 per type). The release threshold.
	assert.True(s.T(), summary.RecallOK,
		"Recall@K gate failed (< 0.9); see report for Missing:\n%s", eval.FormatReport(summary))

	// P95 latency gate (≤ 2000ms, §14.3 SLO).
	assert.True(s.T(), summary.LatencyOK,
		"P95 latency gate failed (> 2000ms): p95=%dms", summary.P95LatencyMS)

	// DoD §4: lock the threshold into context_eval_runs (DatasetTag=phase6-v1).
	// One row per (intent×type) with its Recall/nDCG/Citation/P95, plus a
	// whole-run summary row (intent=NULL, asset_type=NULL).
	s.lockThresholds(ctx, summary)
	s.assertThresholdsLocked(ctx, summary)
}

// lockThresholds writes the per-(intent×type) release scores into
// context_eval_runs with DatasetTag=phase6-v1 (DoD §4). One row per bucket +
// one aggregate row. The report_json carries the full per-case detail.
func (s *ContextEvalSuite) lockThresholds(ctx context.Context, summary eval.Summary) {
	// Clean prior phase6-v1 rows for this dataset so the lock is idempotent.
	_, err := s.pool.Exec(ctx, `DELETE FROM context_eval_runs WHERE dataset_tag = $1`, summary.DatasetTag)
	require.NoError(s.T(), err)

	reportJSON, err := json.Marshal(summaryReportJSON(summary))
	require.NoError(s.T(), err)

	caseCount := eval.CaseCount()
	for _, r := range summary.ByType {
		pass := r.RecallAtK >= r.RecallThreshold && r.CitationAccuracy >= r.CitationThreshold
		_, err := s.pool.Exec(ctx, `
			INSERT INTO context_eval_runs
			       (dataset_tag, intent, asset_type, recall_at_k, ndcg, citation_accuracy, p95_latency_ms, case_count, pass, report_json)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			summary.DatasetTag, string(r.Intent), string(r.AssetType),
			r.RecallAtK, r.NDCG, r.CitationAccuracy, summary.P95LatencyMS,
			caseCount, pass, reportJSON)
		require.NoError(s.T(), err)
	}
	// Aggregate row (intent=NULL, asset_type=NULL): the whole-run pass verdict.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO context_eval_runs
		       (dataset_tag, intent, asset_type, recall_at_k, ndcg, citation_accuracy, p95_latency_ms, case_count, pass, report_json)
		VALUES ($1,NULL,NULL,NULL,NULL,NULL,$2,$3,$4,$5)`,
		summary.DatasetTag, summary.P95LatencyMS, caseCount, summary.Pass(), reportJSON)
	require.NoError(s.T(), err)
}

// assertThresholdsLocked verifies the context_eval_runs rows were written with
// the right DatasetTag + pass verdict (DoD §4 closure check).
func (s *ContextEvalSuite) assertThresholdsLocked(ctx context.Context, summary eval.Summary) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_eval_runs WHERE dataset_tag = $1`, summary.DatasetTag).Scan(&n)
	require.NoError(s.T(), err)
	assert.Greater(s.T(), n, len(summary.ByType),
		"expected ≥ per-bucket rows + an aggregate row in context_eval_runs for %s", summary.DatasetTag)

	// The aggregate row's pass verdict must match summary.Pass() (the release gate).
	var aggPass bool
	err = s.pool.QueryRow(ctx,
		`SELECT pass FROM context_eval_runs WHERE dataset_tag = $1 AND intent IS NULL AND asset_type IS NULL`,
		summary.DatasetTag).Scan(&aggPass)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), summary.Pass(), aggPass,
		"aggregate context_eval_runs.pass must match summary.Pass() (the release gate)")
}

// summaryReportJSON builds the JSONB report payload (per-bucket + per-case
// failures) recorded in context_eval_runs.report_json for audit.
func summaryReportJSON(summary eval.Summary) map[string]any {
	buckets := make([]map[string]any, 0, len(summary.ByType))
	for _, r := range summary.ByType {
		buckets = append(buckets, map[string]any{
			"intent":             string(r.Intent),
			"asset_type":         string(r.AssetType),
			"recall_at_k":        r.RecallAtK,
			"ndcg":               r.NDCG,
			"citation_accuracy":  r.CitationAccuracy,
			"recall_cases":       r.RecallCases,
			"citation_cases":     r.CitationCases,
			"failures":           len(r.Failed),
		})
	}
	return map[string]any{
		"dataset_tag":    summary.DatasetTag,
		"p95_latency_ms": summary.P95LatencyMS,
		"recall_ok":      summary.RecallOK,
		"citation_ok":    summary.CitationOK,
		"latency_ok":     summary.LatencyOK,
		"pass":           summary.Pass(),
		"by_type":        buckets,
		"locked_at":      time.Now().UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// DoD §5: three-state distinguishable + authz-leak-0 on real DB (with the
// seeded dataset), NOT just fake-broker.
// ---------------------------------------------------------------------------

// TestEval_Leak_NoWorkspaceMembership asserts a caller with NO grant on the eval
// workspace gets an EMPTY result — IsEmptyAuthorized==true — even though the
// ports surfaced the dataset's candidates. The workspace gate (step 2) denies
// membership → empty AllowedAssetIDs → post-check denies → 0 candidates. This
// is NOT an error (§11.5): the caller never learns an asset exists (§8.2).
func (s *ContextEvalSuite) TestEval_Leak_NoWorkspaceMembership() {
	ctx := context.Background()
	broker := s.newEvalBroker()
	// A second user with NO permission rows — not a member of the eval workspace.
	bob := uuid.New()
	seedEvalUser(s.T(), s.pool, bob, "eval-bob@mora-eval.local")

	resp, err := broker.Execute(ctx, ctxmod.KnowledgeQuery{
		Query:       "规范要求 接口版本",
		WorkspaceID: s.ds.wsID,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
	}, ctxmod.Principal{SubjectType: domain.SubjectUser, PrincipalID: bob})
	require.NoError(s.T(), err)

	assert.Empty(s.T(), resp.Candidates, "a non-member must get zero candidates")
	assert.True(s.T(), resp.IsEmptyAuthorized(),
		"a denied caller must surface as empty-authorized, not no-results (existence must not leak): %+v", resp)
	assert.False(s.T(), resp.IsProviderDown(), "IsProviderDown must be false (no provider fault)")
}

// TestEval_ThreeState_NoResultsDistinct asserts the genuine no-results state
// (IsEmptyNoResults) is distinguishable from empty-authorized: a query the
// dataset has no asset for (an asset type with no seeded asset in a fresh
// workspace) yields IsEmptyNoResults, NOT IsEmptyAuthorized. This is the §7.3 /
// §11.5 three-state gate.
func (s *ContextEvalSuite) TestEval_ThreeState_NoResultsDistinct() {
	ctx := context.Background()
	broker := s.newEvalBroker()
	// A fresh workspace the owner IS a member of, but has NO seeded assets.
	freshWS := seedEvalWorkspace(s.T(), s.pool, "ws-eval-empty-"+uuid.NewString()[:8], s.ds.owner)
	grantEvalAdmin(s.T(), s.pool, s.ds.owner, s.ds.adminRoleID, freshWS)
	seedEvalAuthzRevision(s.T(), s.pool, freshWS, 7)

	// Rewrite ws to the fresh workspace via a one-off broker wrapper.
	wb := &wsOverride{inner: broker.inner, ws: freshWS, owner: s.ds.owner}
	resp, err := wb.Execute(ctx, ctxmod.KnowledgeQuery{
		Query:       "规范要求 接口版本",
		WorkspaceID: freshWS,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
	}, s.evalPrincipal())
	require.NoError(s.T(), err)

	assert.Empty(s.T(), resp.Candidates)
	// The owner is a member (workspace gate allows) but the ports return empty
	// (no seeded assets) → IsEmptyNoResults, NOT IsEmptyAuthorized.
	assert.True(s.T(), resp.IsEmptyNoResults(),
		"a member querying an empty workspace must surface as no-results, not authorized-empty: %+v", resp)
	assert.False(s.T(), resp.IsProviderDown())
}

// wsOverride is a one-off evalBroker that pins the workspace to a fresh empty
// workspace (for the three-state no-results test). It injects the synthetic
// AgentID (skill delivery, §6.2) like evalBroker; the workspace is pinned to
// the fresh empty workspace so the workspace-scoped retrieval-map ports return
// empty (≠ the seeded eval ws) → IsEmptyNoResults.
type wsOverride struct {
	inner *ctxmod.Broker
	ws    domain.UUID
	owner domain.UUID
}

func (b *wsOverride) Execute(ctx context.Context, q ctxmod.KnowledgeQuery, p ctxmod.Principal) (ctxmod.ContextResponse, error) {
	if q.WorkspaceID == uuid.Nil {
		q.WorkspaceID = b.ws
	}
	if q.AgentID == nil {
		aid := b.owner
		q.AgentID = &aid
	}
	return b.inner.Execute(ctx, q, p)
}
