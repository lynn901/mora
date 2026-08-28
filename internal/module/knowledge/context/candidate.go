// candidate.go — 统一 KnowledgeCandidate（D2）+ ScoredCandidate + RelationSummary。
//
// D2 (§0 决策摘要): all four type-query ports (document/codebase/memory/skill)
// converge their native result shapes onto one KnowledgeCandidate. Memory
// candidates come in through the recall.RecallService adapter; the convergence
// MUST NOT break the already-stable Phase 4 recall contract or its REST
// serialization — the adapter maps fields, recall.KnowledgeCandidate stays as
// is (§13, design-doc 18 不变量). Field set aligns with the §11.3 response
// example: asset_id / asset_type / title / snippet / score / authority /
// freshness / citation / relations.
//
// The four type-query port interfaces (DocumentQuery/CodeQuery/MemoryQuery/
// SkillQuery) live in ports.go; this file holds only the data shapes so the
// candidate type compiles with no context import.

package contextbroker

import (
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// KnowledgeCandidate is the unified result shape every type-query port returns
// (D2). Authority/Freshness/Score are the ranking signals the AuthorityPolicy
// blends (§5.2); Citation + Relations are finalized/normalized by the
// CitationBuilder (§8) and the conflict-preserving dedup (§7.2).
//
// Native port results are mapped to this shape by the per-type adapters; adapters
// do field mapping only, no re-resolution (§8.2). Snippet is redacted and never
// carries Provider credentials (§8.1).
type KnowledgeCandidate struct {
	AssetID   uuid.UUID
	AssetType domain.AssetType
	Title     string
	Snippet   string
	Score     float64
	Authority float64
	Freshness float64
	// Confidence is the per-type native confidence signal (memory carries it;
	// document/code/skill may leave nil). Pointer so "unset" ≠ 0.
	Confidence *float64
	// ContentHash is the dedup key for "same content, different asset" (§7.2).
	ContentHash string
	// Relations carries conflicts (contradicts/old_spec/impl_drift) so the
	// dedup step keeps both sides instead of picking one (§7.2 / §5.1
	// must_surface_conflicts).
	Relations []RelationSummary
	// Citation is the traceable reference; finalized after authorization
	// (§8.2). Builders only map fields the native candidate already carries.
	Citation Citation
	// VersionOrRevision mirrors Citation.VersionOrRevision at the candidate
	// level for adapter convenience: document = version_id; codebase = commit;
	// memory = evidence_id; skill = package_version.
	VersionOrRevision string
}

// RelationSummary is one side of a relation a candidate participates in (12
// §9.3). The conflict types surfaced here (contradicts / supersedes /
// old_spec / impl_drift) are kept side-by-side by DedupAndKeepConflicts when
// the policy's must_surface_conflicts lists them (§7.2) — never silently
// dropped.
type RelationSummary struct {
	RelationType string // supersedes|contradicts|derived_from|related_to|supports|old_spec|impl_drift
	TargetID     uuid.UUID
	TargetTitle  string
}

// ScoredCandidate is a KnowledgeCandidate carrying its blended policy score +
// the rank the AuthorityPolicy assigned (§5.2 Score). The broker feeds scored
// candidates to the Budgeter (§6.2 Select), which truncates by budget — never
// by silently dropping citations (§6.2 / §11.4).
type ScoredCandidate struct {
	KnowledgeCandidate
	Score float64 // blended authority/freshness/confidence/task-match (§9.5)
	Rank  int     // 1-based position within the policy ordering
}
