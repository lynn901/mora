//go:build integration

// eval_dataset_seed.go — YS-227 §9.3 reproducible eval dataset seed harness
// (design-docs/19 §9.1 / §9.3 / §8.1 / §7.2).
//
// This is the fixture seeder the test engineer (YS-227) delivers so eval.Run()
// can score the 18-case DefaultCaseSets against a real PostgreSQL. It seeds the
// dataset the runner's cases_test.go pins with fixed UUID anchors:
//
//   - one eval workspace (ws-eval) + the workspace_admin principal (the runner's
//     fixed eval.evalPrincipal) with full use on the seeded assets (§3.2 — authz
//     is the broker's gate; the principal is admin so the per-asset use post-
//     check allows every seeded asset);
//   - knowledge_assets with the fixed 00000000-...-a000.. / b000.. / c000.. /
//     d000.. UUIDs, one per (intent×type) anchor the cases expect;
//   - knowledge_asset_versions carrying the §8.1 per-type version anchor in
//     source_revision (document=doc-v2, codebase=commit-a1b2c3d,
//     memory=evidence-*, skill=pkg-x.y.z) so the PG-backed ports surface the
//     spec-derived anchor the cases assert (CitationAccuracy judge);
//   - memory_units + memory_evidence + memory_evidence_links for the memory
//     anchors, published so the leak-safe recall default surfaces them;
//   - knowledge_relations for the §7.2 cross-asset contradicts edges the
//     conflict cases assert survive dedup side-by-side;
//   - context_authority_policies rows (the §5.1 built-in configs) so the Broker
//     loads a real DB policy (policy_version ≥ 1), not the built-in default.
//
// Completeness gate (防自造预期, DoD §2): the fixtures are constructed from the
// design-doc ground truth — §8.1 per-type anchors, §7.2 conflict semantics,
// §5.1 four-policy must_surface_conflicts + exclude_when + weights. The
// ExpectedCandidates / ExpectedCitations / ExpectedRelations in cases_test.go
// are derivable from this seed via the spec; nothing here was reverse-fit to
// match broker output. The seed is reproducible (idempotent UPSERT by fixed
// UUID) so CI / the release gate can re-run it.
//
// The cases' WorkspaceID strings ("ws-eval-spec" …) are case-literal labels,
// not UUIDs; the runner's parseWorkspaceID yields uuid.Nil for them. The
// ContextEvalSuite wires the production Broker behind a workspace-resolver
// wrapper (evalBroker) that maps each case's query → the seeded workspace UUID,
// so the broker's workspace-scope authz gate (step 2) runs against the real
// workspace. All five case-literal workspaces consolidate into ONE seeded
// workspace so the fixed-UUID assets are unique PKs and the principal's single
// workspace_admin grant covers the whole dataset.
//
// Build-tagged integration + DATABASE_URL-gated by ContextEvalSuite. NOT a
// standalone test.
package integration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
	"github.com/lynn901/mora/internal/infra/postgres"
)

// evalDataset holds the resolved IDs the seed produces + the suite consumes.
type evalDataset struct {
	wsID       domain.UUID            // the single seeded eval workspace
	wsConflicts domain.UUID           // == wsID (consolidated)
	owner      domain.UUID            // workspace_admin principal (eval.evalPrincipal)
	adminRoleID domain.UUID
	assets     map[domain.UUID]string // fixed UUID → §8.1 anchor (source_revision)
}

// fixed asset UUIDs — MUST match internal/module/knowledge/context/eval/cases_test.go.
// Re-declared (the eval package keeps them unexported); a drift surfaces as a
// recall miss at scoring time, and is pinned by the eval.DefaultCaseSets literal.
var (
	specDocV2     = uuid.MustParse("00000000-0000-0000-0000-a00000000002")
	specDocV1Old  = uuid.MustParse("00000000-0000-0000-0000-a00000000001")
	specCodeDrift = uuid.MustParse("00000000-0000-0000-0000-a00000000003")
	specMemLow    = uuid.MustParse("00000000-0000-0000-0000-a00000000004")

	revCodeCommit  = uuid.MustParse("00000000-0000-0000-0000-b00000000001")
	revDocIncon    = uuid.MustParse("00000000-0000-0000-0000-b00000000002")
	revMemEvidence = uuid.MustParse("00000000-0000-0000-0000-b00000000003")

	ratDocDecision   = uuid.MustParse("00000000-0000-0000-0000-c00000000001")
	ratMemSuperseded = uuid.MustParse("00000000-0000-0000-0000-c00000000002")
	ratMemEvidence   = uuid.MustParse("00000000-0000-0000-0000-c00000000003")

	procSkillV2    = uuid.MustParse("00000000-0000-0000-0000-d00000000001")
	procSkillV1MM  = uuid.MustParse("00000000-0000-0000-0000-d00000000002")
	procDocRunbook = uuid.MustParse("00000000-0000-0000-0000-d00000000003")

	// evalOwnerUUID is the runner's fixed principal (eval.evalPrincipal).
	evalOwnerUUID = uuid.MustParse("00000000-0000-0000-0000-e00000000001")
)

// evalSlug is the single consolidated eval workspace slug.
const evalSlug = "ws-eval"

// seedEvalDataset seeds the full 18-case eval dataset into PG and returns the
// resolved IDs. Idempotent (UPSERT by fixed UUID) so the suite can call it in
// SetupSuite without manual cleanup between runs.
func seedEvalDataset(t require.TestingT, pool *pgxpool.Pool) *evalDataset {
	ds := &evalDataset{
		assets: make(map[domain.UUID]string, 16),
		owner:  evalOwnerUUID,
	}
	ds.adminRoleID = systemRoleID(t, pool, "workspace_admin")
	seedEvalUser(t, pool, ds.owner, "eval-owner@mora-eval.local")

	ds.wsID = seedEvalWorkspace(t, pool, evalSlug, ds.owner)
	ds.wsConflicts = ds.wsID
	grantEvalAdmin(t, pool, ds.owner, ds.adminRoleID, ds.wsID)
	seedEvalAuthzRevision(t, pool, ds.wsID, 7)

	// ---- knowledge_assets + versions (§8.1 per-type anchors) ----
	type assetSeed struct {
		id       domain.UUID
		aType    domain.AssetType
		name     string
		anchor   string // §8.1 version anchor → source_revision
		confTags []string
	}
	assets := []assetSeed{
		// spec dataset
		{specDocV2, domain.AssetTypeDocument, "API spec v2 (current)", "doc-v2", nil},
		{specDocV1Old, domain.AssetTypeDocument, "API spec v1 (old)", "doc-v1", []string{"old_spec"}},
		{specCodeDrift, domain.AssetTypeCodebase, "impl drift code", "commit-a1b2c3d", []string{"impl_drift"}},
		{specMemLow, domain.AssetTypeMemory, "low-confidence memory", "evidence-low", []string{"low_confidence"}},
		// revision dataset
		{revCodeCommit, domain.AssetTypeCodebase, "pinned-commit code", "commit-a1b2c3d", nil},
		{revDocIncon, domain.AssetTypeDocument, "doc/impl inconsistency", "doc-v1", []string{"doc_inconsistency"}},
		{revMemEvidence, domain.AssetTypeMemory, "review evidence memory", "evidence-9f8e", nil},
		// rationale dataset
		{ratDocDecision, domain.AssetTypeDocument, "decision doc", "doc-decision-1", nil},
		{ratMemSuperseded, domain.AssetTypeMemory, "superseded decision memory", "evidence-old", []string{"superseded_memory"}},
		{ratMemEvidence, domain.AssetTypeMemory, "decision evidence memory", "evidence-7a6b", nil},
		// procedure dataset
		{procSkillV2, domain.AssetTypeSkill, "approved skill v2", "pkg-2.0.0", nil},
		{procSkillV1MM, domain.AssetTypeSkill, "version-mismatch skill v1", "pkg-1.0.0", []string{"version_mismatch"}},
		{procDocRunbook, domain.AssetTypeDocument, "runbook doc", "doc-runbook-1", nil},
	}
	for _, a := range assets {
		seedEvalAssetVersion(t, pool, ds.wsID, ds.owner, a.id, a.aType, a.name, a.anchor)
		ds.assets[a.id] = a.anchor
	}

	// ---- memory_units + evidence (memory §8.1 anchor = evidence_id) ----
	// The port adapter surfaces the version's source_revision anchor as
	// Citation.VersionOrRevision; the evidence rows back the §4.3 ACL chain +
	// the leak-safe recall path. specMemLow stays candidate + low authority so
	// the spec/memory "empty expected" case (low-confidence must NOT surface)
	// holds — the recall default returns published-only, and specMemLow is
	// candidate, so recall skips it.
	seedEvalMemory(t, pool, ds.wsID, ds.owner, specMemLow, "low confidence fact spec memory", "evidence-low", 0.2, false)
	seedEvalMemory(t, pool, ds.wsID, ds.owner, revMemEvidence, "review evidence fact revision memory", "evidence-9f8e", 0.8, true)
	seedEvalMemory(t, pool, ds.wsID, ds.owner, ratMemEvidence, "decision evidence fact rationale memory", "evidence-7a6b", 0.8, true)
	seedEvalMemory(t, pool, ds.wsID, ds.owner, ratMemSuperseded, "superseded decision memory rationale", "evidence-old", 0.5, true)

	// ---- knowledge_relations (§7.2 cross-asset contradicts edges) ----
	// Case "实现 contradicts twin" expects specDocV2 carries
	// contradicts→specDocV1Old. Case "为什么 不同资产冲突并排" expects
	// ratDocDecision carries contradicts→ratMemSuperseded.
	seedEvalRelation(t, pool, ds.wsID, ds.owner, specDocV2, specDocV1Old, "contradicts")
	seedEvalRelation(t, pool, ds.wsID, ds.owner, ratDocDecision, ratMemSuperseded, "contradicts")

	// ---- authority policies (§5.1 four built-ins, real DB rows) ----
	polRepo := postgres.NewContextPolicyRepo(postgres.NewDB(pool))
	for _, intent := range []ctxmod.Intent{
		ctxmod.IntentSpec, ctxmod.IntentRevision, ctxmod.IntentRationale, ctxmod.IntentProcedure,
	} {
		seedEvalPolicy(t, polRepo, ds.wsID, intent)
	}
	return ds
}

// anchorFor returns the §8.1 version anchor the port adapter surfaces as
// Citation.VersionOrRevision for a seeded asset.
func (ds *evalDataset) anchorFor(id domain.UUID) string {
	if a, ok := ds.assets[id]; ok {
		return a
	}
	return ""
}

// ---- low-level seed helpers (idempotent UPSERTs by fixed UUID) ----

func systemRoleID(t require.TestingT, pool *pgxpool.Pool, name string) domain.UUID {
	var id domain.UUID
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT id FROM roles WHERE name = $1 AND is_system = true`, name).Scan(&id))
	return id
}

func seedEvalUser(t require.TestingT, pool *pgxpool.Pool, id domain.UUID, email string) {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, name, status) VALUES ($1,$2,$3,'active')
		ON CONFLICT (id) DO NOTHING`, id, email, email)
	require.NoError(t, err)
}

func seedEvalWorkspace(t require.TestingT, pool *pgxpool.Pool, slug string, ownerID domain.UUID) domain.UUID {
	var id domain.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3)
		ON CONFLICT (slug) DO UPDATE SET owner_id = EXCLUDED.owner_id
		RETURNING id`, "WS "+slug, slug, ownerID).Scan(&id)
	require.NoError(t, err)
	return id
}

func grantEvalAdmin(t require.TestingT, pool *pgxpool.Pool, userID, roleID, wsID domain.UUID) {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO permissions (subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope)
		VALUES ('user',$1,$2,'workspace',$3,'allow','subtree')
		ON CONFLICT DO NOTHING`, userID, roleID, wsID)
	require.NoError(t, err)
}

func seedEvalAuthzRevision(t require.TestingT, pool *pgxpool.Pool, wsID domain.UUID, rev int64) {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO workspace_authz_revisions (workspace_id, revision) VALUES ($1,$2)
		ON CONFLICT (workspace_id) DO UPDATE SET revision = EXCLUDED.revision`, wsID, rev)
	require.NoError(t, err)
}

// seedEvalAssetVersion inserts a knowledge_asset (fixed id) + a
// knowledge_asset_versions row carrying the §8.1 anchor in source_revision.
// Idempotent by asset id. The version is build_status=ready,
// governance_status=published so it is the current ready version.
func seedEvalAssetVersion(t require.TestingT, pool *pgxpool.Pool, wsID, ownerID, assetID domain.UUID, aType domain.AssetType, name, anchor string) {
	ctx := context.Background()
	dedupeKey := fmt.Sprintf("eval:%s", assetID)
	_, err := pool.Exec(ctx, `
		INSERT INTO knowledge_assets (id, workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,$2,$3,$4,'user',$5,'published','workspace')
		ON CONFLICT (id) DO UPDATE SET workspace_id = EXCLUDED.workspace_id,
		                               asset_type = EXCLUDED.asset_type,
		                               name = EXCLUDED.name,
		                               status = EXCLUDED.status,
		                               visibility = EXCLUDED.visibility`,
		assetID, wsID, string(aType), name, ownerID)
	require.NoError(t, err)
	var verID domain.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions (asset_id, version_no, source_revision, content_origin, dedupe_key, build_status, governance_status, created_by_type, created_by_id)
		VALUES ($1, 1, $2, 'human', $3, 'ready', 'published', 'user', $4)
		ON CONFLICT (asset_id, version_no) DO UPDATE SET source_revision = EXCLUDED.source_revision,
		                              build_status = EXCLUDED.build_status,
		                              governance_status = EXCLUDED.governance_status
		RETURNING id`, assetID, anchor, dedupeKey, ownerID).Scan(&verID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id = $2 WHERE id = $1`, assetID, verID)
	require.NoError(t, err)
}

// seedEvalMemory seeds a memory_unit (published/candidate) + memory_evidence +
// link for a memory asset, so the recall path + the §4.3 ACL chain resolve.
func seedEvalMemory(t require.TestingT, pool *pgxpool.Pool, wsID, ownerID, assetID domain.UUID, statement, anchor string, authority float64, published bool) {
	ctx := context.Background()
	// memory_evidence storage split CHECK (020): either encrypted_content+key_version
	// OR storage_key. Use storage_key (the large-object path) — no encryption needed.
	storageKey := fmt.Sprintf("mora-evidence/%s/%s", wsID, anchor)
	_, err := pool.Exec(ctx, `
		INSERT INTO memory_evidence (workspace_id, owner_type, owner_id, source_kind, source_ref, captured_authz_revision, content_hash, redacted_excerpt, state, storage_key)
		VALUES ($1,'user',$2,'document',$3,7,$4,$5,'active',$6)
		ON CONFLICT DO NOTHING`,
		wsID, ownerID, anchor, fmt.Sprintf("sha256:%s", anchor), statement, storageKey)
	require.NoError(t, err)
	var evID domain.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM memory_evidence WHERE source_ref = $1 AND workspace_id = $2`, anchor, wsID).Scan(&evID))

	state := string(domain.MemoryPublished)
	if !published {
		state = string(domain.MemoryCandidate)
	}
	var unitID domain.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO memory_units (workspace_id, asset_id, memory_type, statement, state, authority, created_by_type, created_by_id)
		VALUES ($1,$2,'fact',$3,$4,$5,'user',$6)
		ON CONFLICT DO NOTHING
		RETURNING id`, wsID, assetID, statement, state, authority, ownerID).Scan(&unitID)
	if err != nil {
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT id FROM memory_units WHERE asset_id = $1 AND workspace_id = $2`, assetID, wsID).Scan(&unitID))
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO memory_evidence_links (memory_unit_id, evidence_id, support_type)
		VALUES ($1,$2,'supports')
		ON CONFLICT DO NOTHING`, unitID, evID)
	require.NoError(t, err)
}

// seedEvalRelation seeds a cross-asset knowledge_relations edge (contradicts/
// supersedes). Idempotent (ON CONFLICT DO NOTHING).
func seedEvalRelation(t require.TestingT, pool *pgxpool.Pool, wsID, ownerID, fromID, toID domain.UUID, relType string) {
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO knowledge_relations (workspace_id, from_asset_id, relation_type, to_asset_id, origin, created_by_type, created_by_id)
		VALUES ($1,$2,$3,$4,'human','user',$5)
		ON CONFLICT DO NOTHING`, wsID, fromID, relType, toID, ownerID)
	require.NoError(t, err)
}

// seedEvalPolicy upserts the §5.1 built-in config as a real DB policy row so
// the Broker loads policy_version≥1 (not the built-in default at version 0).
func seedEvalPolicy(t require.TestingT, repo ctxmod.PolicyRepo, wsID domain.UUID, intent ctxmod.Intent) {
	_, err := repo.Upsert(context.Background(), ctxmod.AuthorityPolicyRecord{
		WorkspaceID: wsID,
		Intent:      intent,
		IsCurrent:   true,
		Config:      evalPolicyConfigFor(intent),
	})
	require.NoError(t, err)
}

// evalPolicyConfigFor returns the §5.1 built-in config for an intent — the same
// defaults defaultPolicyConfigs carries in policy.go — seeded as a real DB row
// so policy_version ≥ 1. Ground-truth config (DoD §2): derived from §5.1.
func evalPolicyConfigFor(intent ctxmod.Intent) ctxmod.PolicyConfig {
	switch intent {
	case ctxmod.IntentSpec:
		return ctxmod.PolicyConfig{
			PrimaryBasis:         []domain.AssetType{domain.AssetTypeDocument},
			MustSurfaceConflicts: []string{"old_spec", "impl_drift"},
			Weights: map[domain.AssetType]float64{
				domain.AssetTypeDocument: 0.9, domain.AssetTypeCodebase: 0.5,
				domain.AssetTypeMemory: 0.4, domain.AssetTypeSkill: 0.3,
			},
			ExcludeWhen: []string{"deprecated", "version_mismatch"},
		}
	case ctxmod.IntentRevision:
		return ctxmod.PolicyConfig{
			PrimaryBasis:         []domain.AssetType{domain.AssetTypeCodebase},
			MustSurfaceConflicts: []string{"contradicts", "impl_drift"},
			Weights: map[domain.AssetType]float64{
				domain.AssetTypeDocument: 0.5, domain.AssetTypeCodebase: 0.9,
				domain.AssetTypeMemory: 0.3, domain.AssetTypeSkill: 0.4,
			},
			ExcludeWhen: []string{"deprecated", "version_mismatch"},
		}
	case ctxmod.IntentRationale:
		return ctxmod.PolicyConfig{
			PrimaryBasis:         []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
			MustSurfaceConflicts: []string{"contradicts", "old_spec"},
			Weights: map[domain.AssetType]float64{
				domain.AssetTypeDocument: 0.8, domain.AssetTypeCodebase: 0.3,
				domain.AssetTypeMemory: 0.9, domain.AssetTypeSkill: 0.3,
			},
			ExcludeWhen: []string{"deprecated", "version_mismatch"},
		}
	case ctxmod.IntentProcedure:
		return ctxmod.PolicyConfig{
			PrimaryBasis:         []domain.AssetType{domain.AssetTypeSkill, domain.AssetTypeDocument},
			MustSurfaceConflicts: []string{"version_mismatch", "impl_drift"},
			Weights: map[domain.AssetType]float64{
				domain.AssetTypeDocument: 0.6, domain.AssetTypeCodebase: 0.4,
				domain.AssetTypeMemory: 0.4, domain.AssetTypeSkill: 0.9,
			},
			ExcludeWhen: []string{"deprecated"}, // §7.2: procedure must NOT exclude version_mismatch
		}
	}
	return ctxmod.PolicyConfig{}
}
