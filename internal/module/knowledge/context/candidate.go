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

// ---------------------------------------------------------------------------
// Per-type adapter input views + CandidateFrom* mapping helpers (§3.3 / §8.2)
//
// Each adapter wraps a native type engine. The native types (recall.
// KnowledgeCandidate, rag/search.SearchHit, codegraph provider.CodeHit, skill
// DeliveryResult) carry their own per-type sub-structures; the adapter copies
// them into one of these narrow local views and hands it to the corresponding
// CandidateFrom* helper, which builds the unified KnowledgeCandidate. The
// helpers are PURE field mapping — they never re-resolve ids, fetch assets, or
// touch RBAC (§8.2: "不重新解析，只做字段映射与格式统一"). Authorization is
// finalized by the CitationBuilder after the §7.1 step-5 post-check.
//
// Keeping the per-type input views local (not imported from the native
// packages) lets the context package compile against narrow slices of the
// engine surfaces and keeps the adapter unit tests free of DB/infra wiring —
// a fake port returns a local view, the helper maps it, the test asserts the
// unified shape.
// ---------------------------------------------------------------------------

// memoryCandidate is the narrow view of recall.KnowledgeCandidate the memory
// adapter builds (§8.1: evidence locator → Citation.Locator). The adapter
// copies recall.KnowledgeCandidate into this view; CandidateFromMemory builds
// the unified shape. recall.KnowledgeCandidate itself is left untouched (D2 /
// §13: the Phase 4 recall contract + REST serialization is not broken).
type memoryCandidate struct {
	AssetID         uuid.UUID
	AssetVersionID  *uuid.UUID
	Title           string
	Snippet         string
	Score           float64
	Authority       float64
	Freshness       float64
	Confidence      *float64
	ContentHash     string
	EvidenceID      uuid.UUID // memory Citation.VersionOrRevision anchor (§8.1)
	EvidenceLocator map[string]any // §8.1 evidence locator → Citation.Locator
	Relations       []RelationSummary
}

// CandidateFromMemory maps a memory-dimension candidate into the unified shape
// (D2). memory's VersionOrRevision is the evidence_id (§8.1); the evidence
// locator (quote locator) becomes Citation.Locator verbatim — the Broker does
// not re-resolve evidence content (§8.2). The memory candidate's relations
// (incl. contradicts) pass through unchanged so conflicts surface side-by-side
// (§7.2 / §5.1 must_surface_conflicts).
func CandidateFromMemory(m memoryCandidate) KnowledgeCandidate {
	c := KnowledgeCandidate{
		AssetID:           m.AssetID,
		AssetType:         domain.AssetTypeMemory,
		Title:             m.Title,
		Snippet:           m.Snippet,
		Score:             m.Score,
		Authority:         m.Authority,
		Freshness:         m.Freshness,
		Confidence:        m.Confidence,
		ContentHash:       m.ContentHash,
		Relations:         m.Relations,
		Citation: Citation{
			AssetID:  m.AssetID,
			AssetType: domain.AssetTypeMemory,
			Locator:  cloneLocator(m.EvidenceLocator),
		},
	}
	if m.EvidenceID != uuid.Nil {
		c.VersionOrRevision = m.EvidenceID.String()
		c.Citation.VersionOrRevision = c.VersionOrRevision
	}
	if m.AssetVersionID != nil {
		// The asset_version_id is a secondary anchor the CitationBuilder may
		// finalize post-authz (§8.2). The Citation struct carries no dedicated
		// version-id field (§8.1 only names VersionOrRevision per type), so the
		// adapter stashes it in the locator for the Builder to pull; memory's
		// VersionOrRevision is the evidence_id, not the asset_version_id.
		if c.Citation.Locator == nil {
			c.Citation.Locator = make(map[string]any)
		}
		c.Citation.Locator["asset_version_id"] = m.AssetVersionID.String()
	}
	return c
}

// documentHit is the narrow view of a mora/rag search hit the document adapter
// builds (§8.1: document block_id / chunk locator → Citation.Locator). The
// document VersionOrRevision is the version_id, which the search hit does not
// carry (it is resolved at read time); the adapter leaves it empty and the
// CitationBuilder completes it post-authz (§8.2).
type documentHit struct {
	DocumentID uuid.UUID // the knowledge_asset id (document)
	Title      string
	Snippet    string
	Score      float64
	Locator    map[string]any // block_id / chunk_index / section_path (§8.1, §11.3)
}

// CandidateFromDocument maps a document search hit into the unified shape.
// AssetType is document; the locator becomes Citation.Locator verbatim. Score
// is the fused BM25+vector (RRF) score the search engine returned.
func CandidateFromDocument(d documentHit) KnowledgeCandidate {
	loc := cloneLocator(d.Locator)
	return KnowledgeCandidate{
		AssetID:   d.DocumentID,
		AssetType: domain.AssetTypeDocument,
		Title:     d.Title,
		Snippet:   d.Snippet,
		Score:     d.Score,
		// document candidates carry no native authority/freshness/confidence —
		// those are policy-blended later (§5.2). Document freshness can be
		// derived from the asset's updated_at, but the search hit does not carry
		// it; the CitationBuilder finalizes updated_at post-authz (§8.2).
		Citation: Citation{
			AssetID:   d.DocumentID,
			AssetType: domain.AssetTypeDocument,
			Locator:   loc,
		},
	}
}

// codeHit is the narrow view of a codegraph provider.CodeHit the code adapter
// builds (§8.1: code file:line locator → Citation.Locator, commit →
// VersionOrRevision). The adapter copies the ActiveGraph's commit +
// source_tree_ref so every code candidate carries the same revision anchor
// (§4.2: a result without a commit is never returned).
type codeHit struct {
	AssetID        uuid.UUID
	AssetVersionID *uuid.UUID
	Commit         string // VersionOrRevision anchor (§8.1)
	SourceTreeRef  string
	Path           string
	StartLine      int
	EndLine        int
	Symbol         string
	Snippet        string
	Score          float64
}

// CandidateFromCode maps a codegraph hit into the unified shape. The file:line
// locator is built from Path/StartLine/EndLine/Symbol (§8.1 "代码 file:line");
// commit becomes VersionOrRevision (§8.1 "codebase: commit").
func CandidateFromCode(h codeHit) KnowledgeCandidate {
	loc := map[string]any{
		"path": h.Path,
	}
	if h.StartLine > 0 {
		loc["start_line"] = h.StartLine
		if h.EndLine > 0 && h.EndLine != h.StartLine {
			loc["end_line"] = h.EndLine
		}
	}
	if h.Symbol != "" {
		loc["symbol"] = h.Symbol
	}
	if h.SourceTreeRef != "" {
		loc["source_tree_ref"] = h.SourceTreeRef
	}
	c := KnowledgeCandidate{
		AssetID:    h.AssetID,
		AssetType:  domain.AssetTypeCodebase,
		Title:      codeTitle(h.Path, h.Symbol),
		Snippet:    h.Snippet,
		Score:      h.Score,
		Citation: Citation{
			AssetID:           h.AssetID,
			AssetType:         domain.AssetTypeCodebase,
			VersionOrRevision: h.Commit,
			Locator:           loc,
		},
		VersionOrRevision: h.Commit,
	}
	if h.AssetVersionID != nil {
		// stash for the CitationBuilder to finalize post-authz (§8.2); code's
		// VersionOrRevision is the commit, not the asset_version_id.
		c.Citation.Locator["asset_version_id"] = h.AssetVersionID.String()
	}
	return c
}

// codeTitle builds a short title for a code hit. Prefer symbol@path; fall back
// to path; an empty path yields an empty title (the caller down-weights).
func codeTitle(path, symbol string) string {
	if symbol != "" && path != "" {
		return symbol + " @ " + path
	}
	return path
}

// skillHit is the narrow view of a skill DeliveryResult the skill adapter builds
// (§8.1: skill resource locator → Citation.Locator, package_version →
// VersionOrRevision). The adapter trims the surface by the binding
// delivery_mode (§3.3): tool = SKILL.md head (Header), summary = description,
// inline = resource list (Manifest files).
type skillHit struct {
	AssetID           uuid.UUID
	AssetVersionID    *uuid.UUID
	Title             string // SKILL.md name (Header["name"])
	Snippet           string // description, by delivery_mode projection
	VersionOrRevision string // package_version (§8.1)
	Locator           map[string]any // delivery_mode + resource list (§3.3 inline)
	ContentHash       string
}

// CandidateFromSkill maps a skill delivery result into the unified shape. The
// locator carries the delivery_mode so the Broker/Budgeter know how the skill
// was surfaced (§3.3). Skill's VersionOrRevision is the package_version (§8.1).
func CandidateFromSkill(s skillHit) KnowledgeCandidate {
	c := KnowledgeCandidate{
		AssetID:           s.AssetID,
		AssetType:         domain.AssetTypeSkill,
		Title:             s.Title,
		Snippet:           s.Snippet,
		ContentHash:       s.ContentHash,
		VersionOrRevision: s.VersionOrRevision,
		Citation: Citation{
			AssetID:           s.AssetID,
			AssetType:         domain.AssetTypeSkill,
			VersionOrRevision: s.VersionOrRevision,
			Locator:           cloneLocator(s.Locator),
		},
	}
	if s.AssetVersionID != nil {
		// stash for the CitationBuilder to finalize post-authz (§8.2); skill's
		// VersionOrRevision is the package_version, not the asset_version_id.
		if c.Citation.Locator == nil {
			c.Citation.Locator = make(map[string]any)
		}
		c.Citation.Locator["asset_version_id"] = s.AssetVersionID.String()
	}
	return c
}

// cloneLocator returns a shallow copy of a locator map so the candidate's
// Citation.Locator is not shared with the adapter's transient view (defence
// against a later caller mutating the source). nil stays nil.
func cloneLocator(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
