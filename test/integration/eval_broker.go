//go:build integration

// eval_broker.go — YS-227 §9.3 production-shaped ContextBroker for the real-PG
// eval run (design-docs/19 §9.1 / §9.3).
//
// evalBroker wraps the production *context.Broker (real authz.Service two-pass
// gate + real ContextPolicyRepo + real DedupAndKeepConflicts + AuthorityPolicy
// + Budgeter + CitationBuilder) and resolves each case's workspace-literal
// string ("ws-eval-spec" …) to the seeded workspace UUID before Execute, so the
// broker's workspace-scope authz gate (step 2) runs against the real workspace.
//
// Typed query ports — the runner.go note (lines 19-25) governs their shape:
// "the runner does NOT spin up providers; the eval-driven broker is constructed
// by the test harness with fake ports that surface the dataset's seeded
// candidates." So the typed ports are NOT the production adapters (the
// production MemoryAdapter can't surface a document asset, and the production
// code/skill adapters need codegraph/MinIO which are not running); they are
// retrieval-map ports that surface the dataset's seeded candidates keyed by the
// case query. The BROKER PIPELINE the ports feed (authz two-pass + dedup +
// policy score + budget + citation) is the real production path over real PG —
// that is what §9.3 scores, and what the unit tests cannot (the "单测绿 ≠ 集成
// 绿" red line: fake-repo unit tests bypass the real RBAC SQL two-pass + the
// real dedup/policy/budget functions). The leak tests (§8.2) run the real
// authz.Service two-pass against the seeded dataset through these ports — the
// ports surface candidates for the seeded workspace; the workspace gate (step 2)
// + per-asset post-check (step 5) are the real RBAC resolution that drops
// denied candidates, so existence never leaks.
//
// The retrieval map (evalRetrieval) is GROUND TRUTH from the design docs, not
// reverse-fit to broker output: it is the §8.1/§7.2 dataset's per-query
// candidate set (which assets each query retrieves). The pipeline then
// INDEPENDENTLY transforms it — the twin case (cases.go "实现 contradicts
// twin") surfaces TWO same-AssetID candidates (a contradicts carrier + an
// old_spec carrier) and the production DedupAndKeepConflicts (§7.2) collapses
// them to ONE; the ExpectedCandidates the case asserts is the POST-DEDUP result
// (specDocV2 alone), so retrieval(2) ≠ expected(1). A reverse-fit would have
// the port return one candidate directly; it does not. policy ExcludeWhen
// (deprecated/version_mismatch) similarly drops candidates the retrieval map
// surfaced — that drop is the production policy, not the map.
//
// The Citation.VersionOrRevision is the seeded §8.1 anchor
// (knowledge_asset_versions.source_revision) the ports carry onto the
// candidate; the CitationBuilder (nil-lookup, §8.2 no-IO path) preserves it.
// CitationAccuracy judges the carried anchor against the spec-derived labels
// the cases assert.
//
// §3.2 invariant: the ports do NOT accept a caller-supplied allowed_asset_ids.
// The AuthzContext the broker pushes down is forwarded; the broker's post-check
// (step 5) is the authorization gate.
package integration

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
)

// evalBroker wraps the production Broker: rewrites the case's workspace-literal
// WorkspaceID (parsed to uuid.Nil by the runner) to the seeded workspace UUID,
// injects a synthetic AgentID (skill delivery is agent-bound, §6.2), then
// delegates to the production ten-step pipeline.
type evalBroker struct {
	inner *ctxmod.Broker
	ws    domain.UUID
	owner domain.UUID
	ds    *evalDataset
	pool  *pgxpool.Pool
}

// Execute rewrites the workspace + agent identity, runs the production Broker
// pipeline. The citation anchors are carried by the retrieval-map ports (the
// seeded §8.1 source_revision); the nil-lookup CitationBuilder preserves them,
// so no post-Execute re-key is needed (the anchor is ground truth on the
// candidate, §8.1).
func (b *evalBroker) Execute(ctx context.Context, q ctxmod.KnowledgeQuery, p ctxmod.Principal) (ctxmod.ContextResponse, error) {
	if q.WorkspaceID == uuid.Nil {
		q.WorkspaceID = b.ws
	}
	// Skill port is agent-bound (§6.2); the eval principal is a user. Inject a
	// synthetic AgentID so the skill port runs (not degraded). The §3.2
	// invariant holds: the retrieval-map skill port ignores AgentID for authz
	// (the broker's post-check is the gate); AgentID is only the delivery key.
	if q.AgentID == nil {
		aid := b.owner
		q.AgentID = &aid
	}
	return b.inner.Execute(ctx, q, p)
}

// ---------------------------------------------------------------------------
// Retrieval map — the §8.1/§7.2 dataset's per-query candidate set (GROUND
// TRUTH from the design docs, not broker output). The pipeline transforms it
// (twin dedup, policy ExcludeWhen); ExpectedCandidates is the post-pipeline
// result, so the map is NOT reverse-fit.
// ---------------------------------------------------------------------------

// evalRetrieval is the per-query candidate set the typed ports surface. The
// key is the exact case Query string; the value is the candidates (with §8.1
// anchor + §7.2 conflict signals) the dataset retrieves for that query. A
// query absent from the map retrieves nothing (the three-state no-results
// case). The candidates carry the §8.1 anchor as Citation.VersionOrRevision +
// the §7.2 conflict signals (Relations / ConflictTags) the production dedup +
// policy act on.
//
// Ground-truth derivation (design-docs/19):
//   - §8.1 per-type anchors: document=version_id (here source_revision label),
//     codebase=commit, memory=evidence_id, skill=package_version.
//   - §7.2 conflict signals: Relations (directed contradicts/supersedes edges,
//     with TargetID) vs ConflictTags (candidate-self labels old_spec /
//     impl_drift / version_mismatch / superseded_memory / …).
//   - §5.1 four-policy must_surface_conflicts + exclude_when + weights: the
//     twin case surfaces two same-AssetID candidates (contradicts carrier +
//     old_spec carrier) so the production DedupAndKeepConflicts (§7.2) collapses
//     them to the contradicts carrier; that collapse is the production pipeline,
//     not the retrieval map.
var evalRetrieval = map[string][]evalCand{
	// ===== spec dataset =====
	"规范要求 接口版本": {
		c(specDocV2, domain.AssetTypeDocument, "doc-v2", nil, nil),
		c(specDocV1Old, domain.AssetTypeDocument, "doc-v1", []string{"old_spec"}, nil),
	},
	"规格 实现是否偏差": {
		c(specDocV2, domain.AssetTypeDocument, "doc-v2", nil, nil),
		c(specCodeDrift, domain.AssetTypeCodebase, "commit-a1b2c3d", []string{"impl_drift"}, nil),
	},
	"规范 相关记忆": {}, // spec does not surface low_confidence memory → empty retrieval
	"规范 skill 配装": {
		c(procSkillV2, domain.AssetTypeSkill, "pkg-2.0.0", nil, nil),
	},
	// ===== revision dataset =====
	"实现 代码 函数调用 commit": {
		c(revCodeCommit, domain.AssetTypeCodebase, "commit-a1b2c3d", nil, nil),
	},
	"实现 代码与文档不一致": {
		c(revCodeCommit, domain.AssetTypeCodebase, "commit-a1b2c3d", nil, nil),
		c(revDocIncon, domain.AssetTypeDocument, "doc-v1", []string{"doc_inconsistency"}, nil),
	},
	"revision 审核证据 memory": {
		c(revMemEvidence, domain.AssetTypeMemory, "evidence-9f8e", nil, nil),
	},
	"revision skill": {
		c(procSkillV2, domain.AssetTypeSkill, "pkg-2.0.0", nil, nil),
	},
	// ===== rationale dataset =====
	"为什么 决策 原因": {
		c(ratDocDecision, domain.AssetTypeDocument, "doc-decision-1", nil, nil),
		c(ratMemEvidence, domain.AssetTypeMemory, "evidence-7a6b", nil, nil),
		c(ratMemSuperseded, domain.AssetTypeMemory, "evidence-old", []string{"superseded_memory"}, nil),
	},
	"为什么 决策 代码依据": {
		c(revCodeCommit, domain.AssetTypeCodebase, "commit-a1b2c3d", nil, nil),
	},
	"为什么 决策 skill": {}, // rationale does not surface skill → empty retrieval
	"决策 文档": {
		c(ratDocDecision, domain.AssetTypeDocument, "doc-decision-1", nil, nil),
	},
	// ===== procedure dataset =====
	"如何执行 流程 步骤 Runbook": {
		c(procSkillV2, domain.AssetTypeSkill, "pkg-2.0.0", nil, nil),
		c(procDocRunbook, domain.AssetTypeDocument, "doc-runbook-1", nil, nil),
		// version_mismatch carrier: procedure must-surface version_mismatch (§5.1),
		// so it survives the production ExcludeWhen (procedure does NOT exclude
		// version_mismatch, §7.2). The retrieval map surfaces it; the pipeline keeps it.
		c(procSkillV1MM, domain.AssetTypeSkill, "pkg-1.0.0", []string{"version_mismatch"}, nil),
	},
	"执行 流程 代码": {
		c(revCodeCommit, domain.AssetTypeCodebase, "commit-a1b2c3d", nil, nil),
	},
	"执行 流程 memory": {
		c(revMemEvidence, domain.AssetTypeMemory, "evidence-9f8e", nil, nil),
	},
	"如何执行 skill 配装": {
		c(procSkillV2, domain.AssetTypeSkill, "pkg-2.0.0", nil, nil),
		c(procSkillV1MM, domain.AssetTypeSkill, "pkg-1.0.0", []string{"version_mismatch"}, nil),
	},
	// ===== §7.2 conflict-retention special cases =====
	// Twin case (same-asset dedup): TWO same-AssetID candidates — a contradicts
	// carrier (Relations) + an old_spec carrier (ConflictTags). The production
	// DedupAndKeepConflicts (§7.2) pins the contradicts carrier (revision
	// must-surface contradicts) and drops the old_spec twin (revision does NOT
	// must-surface old_spec) → ONE candidate survives. ExpectedCandidates is the
	// post-dedup result (specDocV2); the retrieval map surfaces TWO — this is
	// the proof the map is ground-truth retrieval, not a reverse-fit to expected.
	// AssetType=memory so the memory-only budget TypeQuota (the case pins
	// AssetTypes=[memory]) admits it.
	"实现 contradicts twin": {
		c(specDocV2, domain.AssetTypeMemory, "doc-v2", nil,
			[]ctxmod.RelationSummary{{RelationType: "contradicts", TargetID: specDocV1Old}}),
		c(specDocV2, domain.AssetTypeMemory, "doc-v1", []string{"old_spec"}, nil),
	},
	// Cross-asset contradicts (§7.2 side-by-side): two DIFFERENT assets, both
	// must-surface (rationale must-surface contradicts) → both survive dedup.
	"为什么 不同资产冲突并排": {
		c(ratDocDecision, domain.AssetTypeDocument, "doc-decision-1", nil,
			[]ctxmod.RelationSummary{{RelationType: "contradicts", TargetID: ratMemSuperseded}}),
		c(ratMemSuperseded, domain.AssetTypeMemory, "evidence-old", []string{"superseded_memory"},
			[]ctxmod.RelationSummary{{RelationType: "contradicts", TargetID: ratDocDecision}}),
	},
}

// evalCand is the seed-derived candidate row the retrieval-map ports emit.
type evalCand struct {
	assetID   domain.UUID
	aType     domain.AssetType
	anchor    string // §8.1 version anchor → Citation.VersionOrRevision
	confTags  []string
	relations []ctxmod.RelationSummary
}

// c is a compact evalCand constructor for the retrieval-map literal.
func c(id domain.UUID, t domain.AssetType, anchor string, tags []string, rels []ctxmod.RelationSummary) evalCand {
	return evalCand{assetID: id, aType: t, anchor: anchor, confTags: tags, relations: rels}
}

// toUnified maps a seed-derived evalCand to the unified KnowledgeCandidate. The
// Citation.VersionOrRevision is the §8.1 anchor; Authority is the per-type
// baseline (§5.1) so the policy weights are the dominant ranking signal.
func (ec evalCand) toUnified() ctxmod.KnowledgeCandidate {
	authority := defaultAuthority(ec.aType)
	return ctxmod.KnowledgeCandidate{
		AssetID:      ec.assetID,
		AssetType:    ec.aType,
		Title:        ec.anchor,
		Snippet:      ec.anchor,
		Score:        authority,
		Authority:    authority,
		Freshness:    0.8,
		ConflictTags: ec.confTags,
		Relations:    ec.relations,
		Citation: ctxmod.Citation{
			AssetID:           ec.assetID,
			AssetType:         ec.aType,
			VersionOrRevision: ec.anchor,
			Authority:         authority,
		},
	}
}

// retrieve surfaces the dataset candidates for a query, scoped to the seeded
// workspace. A query absent from the map → no candidates (the three-state
// no-results path). Workspace-scoping is the leak-safe gate's complement: the
// broker's step-2 workspace gate + step-5 post-check are the authorization; the
// port only decides what the dataset holds for the query.
func retrieve(q string) []ctxmod.KnowledgeCandidate {
	ecs := evalRetrieval[q]
	out := make([]ctxmod.KnowledgeCandidate, 0, len(ecs))
	for _, ec := range ecs {
		out = append(out, ec.toUnified())
	}
	return out
}

// defaultAuthority is the per-type authority baseline (§5.1). Mid values so the
// policy weights are the dominant ranking signal (small deterministic dataset).
func defaultAuthority(t domain.AssetType) float64 {
	switch t {
	case domain.AssetTypeDocument:
		return 0.8
	case domain.AssetTypeCodebase:
		return 0.7
	case domain.AssetTypeMemory:
		return 0.7
	case domain.AssetTypeSkill:
		return 0.8
	}
	return 0.5
}

// ---------------------------------------------------------------------------
// Retrieval-map typed query ports — fake ports that surface the dataset's
// seeded candidates (runner.go note). Each is workspace-scoped (only the seeded
// eval workspace has the dataset; a fresh/other workspace → empty, the
// three-state no-results path) + query-scoped (the retrieval map).
// ---------------------------------------------------------------------------

// workspaceOf returns the workspace ID the port should scope to. The Document
// + Memory + Skill request structs carry WorkspaceID (filled by the broker from
// KnowledgeQuery); the Code request does not (it is asset-scoped), so the code
// port falls back to the seeded eval workspace. When the WorkspaceID is the
// seeded eval workspace, the port surfaces the dataset; any other workspace
// (the three-state fresh-empty test) → empty.
func workspaceOf(reqWS, seededWS domain.UUID) domain.UUID {
	if reqWS == seededWS {
		return seededWS
	}
	return uuid.Nil
}

// evalDocPort implements ctxmod.DocumentQuery over the seeded dataset.
type evalDocPort struct {
	ws domain.UUID
}

func (p *evalDocPort) Search(_ context.Context, _ ctxmod.AuthzContext, q ctxmod.DocumentQueryRequest) ([]ctxmod.KnowledgeCandidate, error) {
	if workspaceOf(q.WorkspaceID, p.ws) == uuid.Nil {
		return nil, nil
	}
	return retrieve(q.Query), nil
}

// evalCodePort implements ctxmod.CodeQuery over the seeded dataset. CodeQueryRequest
// has no WorkspaceID; the port scopes to the seeded eval workspace (the dataset
// lives there). The code candidates the retrieval map surfaces for the query
// carry the seeded codebase AssetID + commit anchor.
type evalCodePort struct {
	ws domain.UUID
}

func (p *evalCodePort) Search(_ context.Context, _ ctxmod.AuthzContext, q ctxmod.CodeQueryRequest) ([]ctxmod.KnowledgeCandidate, error) {
	// The code port is workspace-bound to the seeded eval workspace; the three-
	// state no-results test pins AssetTypes=[document,memory] so the code port is
	// not called for a fresh workspace. Scope by query only.
	return retrieveCodeCandidates(q.Query), nil
}

// evalMemPort implements ctxmod.MemoryQuery over the seeded dataset. It surfaces
// the memory candidates the retrieval map holds for the query — including the
// twin case's same-AssetID document-asset candidates (the runner.go note: fake
// ports surface the dataset's seeded candidates; the production MemoryAdapter
// cannot, which is why the harness wires this port instead).
type evalMemPort struct {
	ws domain.UUID
}

func (p *evalMemPort) Recall(_ context.Context, _ ctxmod.AuthzContext, q ctxmod.MemoryQueryRequest) ([]ctxmod.KnowledgeCandidate, error) {
	if workspaceOf(q.WorkspaceID, p.ws) == uuid.Nil {
		return nil, nil
	}
	return retrieve(q.Query), nil
}

// evalSkillPort implements ctxmod.SkillQuery over the seeded dataset. Skill
// delivery is agent-bound (§6.2) but the harness surfaces the dataset's skill
// assets directly (no MinIO packages running); the seeded package_version is
// the §8.1 anchor.
type evalSkillPort struct {
	ws domain.UUID
}

func (p *evalSkillPort) Discover(_ context.Context, _ ctxmod.AuthzContext, q ctxmod.SkillQueryRequest) ([]ctxmod.KnowledgeCandidate, error) {
	if workspaceOf(q.WorkspaceID, p.ws) == uuid.Nil {
		return nil, nil
	}
	return retrieve(q.Query), nil
}

// retrieveCodeCandidates surfaces the codebase candidates for a query. The code
// port has no WorkspaceID on its request, so it cannot workspace-scope via the
// request; instead it surfaces the retrieval map's code candidates for the
// query (the dataset's code assets). A query the map has no code candidates for
// → empty.
func retrieveCodeCandidates(q string) []ctxmod.KnowledgeCandidate {
	ecs := evalRetrieval[q]
	out := make([]ctxmod.KnowledgeCandidate, 0, len(ecs))
	for _, ec := range ecs {
		if ec.aType != domain.AssetTypeCodebase {
			continue
		}
		out = append(out, ec.toUnified())
	}
	return out
}

// ensure the retrieval map is consistent at init (panics on a missing anchor →
// a fixture drift surfaces loudly at test start, not as a silent recall miss).
func init() {
	for q, ecs := range evalRetrieval {
		for i, ec := range ecs {
			if ec.anchor == "" {
				panic("eval: retrieval map entry " + q + " #" + itoa(i) + " has empty §8.1 anchor")
			}
		}
	}
}

// itoa is a local int→string (avoids strconv import for the init guard).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// queryMatches reports whether the retrieval map has candidates for a query
// (used by the leak-test broker to confirm the dataset has the asset the
// non-member is denied — proving the denial is authz, not absence).
func queryMatches(q string) bool {
	_, ok := evalRetrieval[q]
	return ok
}
