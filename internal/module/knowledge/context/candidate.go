// Package contextbroker is the Phase 6 Context Broker — the delivery-convergence
// layer of the knowledge base (design-docs/19-phase6-context-broker.md, doc 12
// §3.1 目录预留 knowledge/context). It routes a query by intent to typed query
// ports, runs parallel fetches under a shared deadline, dedups while keeping
// conflicts, scores under a versioned authority policy, trims to a budget, and
// returns candidates with traceable citations — never bypassing platform/authz
// (§3.2: can orchestrate typed query ports but MUST NOT bypass platform/authz).
//
// This file defines the unified KnowledgeCandidate (D2): the single cross-type
// delivery shape. Existing type-specific candidate shapes — notably the memory
// dimension recall.KnowledgeCandidate (Phase 4, §18) — are NOT mutated; each
// type's adapter converts into this unified shape. This keeps Phase 4's REST
// serialization stable (YS-98 done) while giving the Broker one shape to rank,
// dedup, and cite across document/codebase/memory/skill.
package contextbroker

import (
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// KnowledgeCandidate is the unified cross-type delivery shape (doc 12 §9.3,
// design-docs/19 §0 D2 / §11.3). One shape the Broker ranks, dedups, budgets,
// and cites regardless of whether the source is a document, codebase, memory,
// or skill. Field names align with the §11.3 response example.
//
// Type-specific candidates (memory recall.KnowledgeCandidate) carry dimensions
// the unified shape collapses (e.g. MemoryType, UnitID, State). Those survive
// inside the type's own surface and REST contract; an adapter maps the type
// candidate onto this shape for Broker-internal orchestration only.
type KnowledgeCandidate struct {
	// AssetID is the knowledge_assets.id this candidate speaks for (the stable
	// cross-type identity; §1.2 013 knowledge_core). For a memory candidate this
	// is the memory asset (unit.asset_id), NOT unit.id.
	AssetID uuid.UUID `json:"asset_id"`
	// AssetVersion is the knowledge_asset_versions.id (document: version_id;
	// codebase: the version the active graph is bound to; memory: evidence
	// version; skill: the delivered package version). May be nil when the source
	// engine did not resolve a version (Broker treats nil as "unversioned").
	AssetVersion *uuid.UUID `json:"asset_version_id,omitempty"`
	// AssetType is one of document/codebase/memory/skill (domain.AssetType).
	AssetType domain.AssetType `json:"asset_type"`
	// Title is a human label for the candidate (document title, symbol/symbol+
	// path for code, memory statement lead, skill name). Never carries
	// credentials (§8.1).
	Title string `json:"title"`
	// Snippet is a redacted excerpt (§8.1) — the statement, a code fragment, a
	// search highlight, or the skill description. Never the full body (§6.2
	// default is directory + summary + citation; the body is a progressive read).
	Snippet string `json:"snippet"`
	// Score is the blended authority/freshness/confidence ranking the producing
	// engine emitted; the Broker's AuthorityPolicy re-scores on top of this
	// (§5.2 Score). Provider score is preserved as the input signal.
	Score float64 `json:"score"`
	// Authority is the type-engine authority signal in [0,1] (memory:
	// memory_units.authority; document: governance; code: revision weight;
	// skill: approval). Policy weights blend with this (§5.1).
	Authority float64 `json:"authority"`
	// Freshness is the temporal recency signal in [0,1] (§9.5). 1.0 = newest.
	Freshness float64 `json:"freshness"`
	// Confidence is the producing engine's self-confidence (memory evidence
	// density, search rank certainty). Nil when the engine emits no signal.
	Confidence *float64 `json:"confidence,omitempty"`
	// ContentHash dedups same-content-different-asset candidates (§7.2). Empty
	// when the source engine did not compute one (memory/code may omit).
	ContentHash string `json:"content_hash,omitempty"`
	// Relations carries conflict relations (contradicts/supersedes) so the
	// Broker keeps them side-by-side instead of picking one (D7, §7.2). The
	// policy's ConflictsToSurface decides which survive budgeting.
	Relations []RelationSummary `json:"relations,omitempty"`
	// Citation is the traceable reference (D9, §8.1). The type adapter pre-fills
	// what the source engine already carries; the Broker's CitationBuilder does
	// the final authorized field completion (§8.2 — does not re-parse).
	Citation Citation `json:"citation"`
}

// RelationSummary is one side of a relation a candidate participates in
// (doc 12 §9.3, mirroring recall.RelationSummary). The Broker surfaces
// contradicts/supersedes so conflicts are not silently chosen (§7.2, §9.5
// must_surface_conflicts).
type RelationSummary struct {
	RelationType string `json:"relation_type"` // supersedes|contradicts|derived_from|related_to|supports
	TargetID     uuid.UUID `json:"target_id"`
	TargetTitle  string `json:"target_title"`
}

// Citation is the unified traceable reference (D9, doc 11 §7.4 / 12 §9.3).
// The type adapter fills the fields the source engine already has; the
// CitationBuilder completes source_ref / version_or_revision / updated_at /
// locator AFTER the final authorization pass (§8.2). It never carries Provider
// credentials or storage addresses (§8.1 — ProjectionRef is internal-only and
// is NOT present on this shape).
type Citation struct {
	// AssetID mirrors the candidate's AssetID (denormalized so a citation is
	// self-contained for audit/logging, §9.3).
	AssetID uuid.UUID `json:"asset_id"`
	// AssetType mirrors the candidate's AssetType.
	AssetType domain.AssetType `json:"asset_type"`
	// SourceRef is the sanitized source name/url (no credentials, §8.1).
	SourceRef string `json:"source_ref,omitempty"`
	// VersionOrRevision is the version anchor: document=version_id, codebase=
	// commit, memory=evidence_id, skill=package_version (§8.1). Free-form string
	// because the anchor kind differs per type.
	VersionOrRevision string `json:"version_or_revision,omitempty"`
	// UpdatedAt is the asset/version's last-update time (for freshness ranking +
	// citation display). Zero-value when the source engine did not resolve it.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// Authority is the per-citation authority (denormalized for audit, §8.1).
	Authority float64 `json:"authority"`
	// Confidence mirrors the candidate confidence on the citation (§8.1).
	Confidence *float64 `json:"confidence,omitempty"`
	// Locator is the precise position: document block_id, code file:line,
	// memory conversation message, skill resource path (§8.1). Free-form map so
	// each type's locator shape is preserved without a union type.
	Locator map[string]any `json:"locator,omitempty"`
}

// ScoredCandidate is a candidate plus the policy's blended score (§5.2
// AuthorityPolicy.Score). The Broker ranks by Score, then dedups/budgets.
type ScoredCandidate struct {
	Candidate KnowledgeCandidate
	Score     float64 // policy-blended score (authority/freshness/confidence + weights)
}

// CandidateFromMemory maps a memory-dimension recall.KnowledgeCandidate into
// the unified shape WITHOUT mutating the source (D2 adapter, §3.3). The memory
// candidate carries UnitID/MemoryType/State which have no slot on the unified
// shape; those stay on the memory surface and its REST contract. The evidence
// locator is preserved on Citation.Locator; the evidence id becomes the
// VersionOrRevision anchor (§8.1 memory=evidence_id).
//
// This is a pure conversion — no authz, no IO. The Broker calls it after the
// post-check pass; adapters are not a gate (§3.2 invariants live on the ports).
func CandidateFromMemory(m memoryCandidate) KnowledgeCandidate {
	c := KnowledgeCandidate{
		AssetID:      m.AssetID,
		AssetType:     domain.AssetTypeMemory,
		Title:        m.Title,
		Snippet:      m.Snippet,
		Score:        m.Score,
		Authority:    m.Authority,
		Freshness:    m.Freshness,
		Confidence:   m.Confidence,
		ContentHash:  m.ContentHash,
		Relations:    toUnifiedRelations(m.Relations),
		Citation: Citation{
			AssetID:     m.AssetID,
			AssetType:    domain.AssetTypeMemory,
			Authority:   m.Authority,
			Confidence:  m.Confidence,
			Locator:     m.EvidenceLocator,
		},
	}
	// Evidence id is the memory version anchor (§8.1). Only set when present —
	// a unit with no surviving evidence link (evidence_missing) still cites the
	// unit, just without an evidence id.
	if m.EvidenceID != uuid.Nil {
		c.Citation.VersionOrRevision = m.EvidenceID.String()
	}
	if m.AssetVersionID != nil {
		c.AssetVersion = m.AssetVersionID
	}
	return c
}

// memoryCandidate is the narrow view of recall.KnowledgeCandidate the memory
// adapter extracts. It is a local struct (not an import of the recall package's
// full candidate) so the context package stays decoupled from the memory
// module's wider type surface — the adapter at the wiring layer constructs it
// from the real recall.KnowledgeCandidate. This mirrors the narrow-port
// precedent (authz.SourceRepo, recall.UnitReader).
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
	Relations       []recallRelation
	EvidenceID      uuid.UUID
	EvidenceLocator map[string]any
}

// recallRelation is the narrow relation view (mirrors recall.RelationSummary).
type recallRelation struct {
	RelationType string
	TargetID     uuid.UUID
	TargetTitle  string
}

func toUnifiedRelations(rs []recallRelation) []RelationSummary {
	if len(rs) == 0 {
		return nil
	}
	out := make([]RelationSummary, len(rs))
	for i, r := range rs {
		out[i] = RelationSummary{
			RelationType: r.RelationType,
			TargetID:     r.TargetID,
			TargetTitle:  r.TargetTitle,
		}
	}
	return out
}

// CandidateFromDocument maps a mora/search document hit into the unified shape
// (§3.3 DocumentQuery adapter). The document carries version_id as its version
// anchor and block_id (optional) as the locator. RBAC is unchanged: the adapter
// does NOT accept user-submitted allowed_asset_ids — only the server-built
// AuthzContext (§3.2 invariant, enforced at the port, not here).
func CandidateFromDocument(d documentHit) KnowledgeCandidate {
	c := KnowledgeCandidate{
		AssetID:   d.DocumentID,
		AssetType:  domain.AssetTypeDocument,
		Title:     d.Title,
		Snippet:   d.Snippet,
		Score:     d.Score,
		Freshness: d.Freshness,
		Citation: Citation{
			AssetID:    d.DocumentID,
			AssetType:   domain.AssetTypeDocument,
			Authority:  d.Authority,
			Locator:    d.Locator,
		},
	}
	if d.VersionID != nil {
		c.AssetVersion = d.VersionID
		c.Citation.VersionOrRevision = d.VersionID.String()
	}
	return c
}

// documentHit is the narrow view of a mora/search Result + the rag/search
// SearchHit the DocumentQuery adapter extracts.
type documentHit struct {
	DocumentID uuid.UUID
	VersionID  *uuid.UUID
	Title      string
	Snippet    string
	Score      float64
	Authority  float64
	Freshness  float64
	Locator    map[string]any // block_id / chunk_index / section_path
}

// CandidateFromCode maps a codegraph CodeHit into the unified shape (§3.3
// CodeQuery adapter). The code anchor is commit + file:line; the active
// graph's source_tree_ref is carried on Citation.SourceRef (sanitized). The
// adapter only reads — it never triggers a build (§3.2).
func CandidateFromCode(c codeHit) KnowledgeCandidate {
	loc := map[string]any{
		"file":       c.Path,
		"start_line": c.StartLine,
	}
	if c.EndLine > 0 {
		loc["end_line"] = c.EndLine
	}
	if c.Symbol != "" {
		loc["symbol"] = c.Symbol
	}
	return KnowledgeCandidate{
		AssetID:   c.AssetID,
		AssetType:  domain.AssetTypeCodebase,
		Title:     codeTitle(c),
		Snippet:   c.Snippet,
		Score:     c.Score,
		Freshness: c.Freshness,
		Citation: Citation{
			AssetID:           c.AssetID,
			AssetType:          domain.AssetTypeCodebase,
			SourceRef:         c.SourceTreeRef,
			VersionOrRevision: c.Commit,
			Locator:           loc,
		},
	}
}

func codeTitle(c codeHit) string {
	if c.Symbol != "" {
		return c.Symbol
	}
	return c.Path
}

// codeHit is the narrow view of a codegraph CodeHit + its host codebase
// asset identity + active-graph commit/source_tree_ref.
type codeHit struct {
	AssetID       uuid.UUID
	Commit        string
	SourceTreeRef string
	Path          string
	StartLine     int
	EndLine       int
	Symbol        string
	Snippet        string
	Score         float64
	Freshness     float64
}

// CandidateFromSkill maps a skill delivery result into the unified shape (§3.3
// SkillQuery adapter). The version anchor is the delivered package version
// (package_version / version_no); the locator carries the resource path or the
// SKILL.md header depending on binding delivery_mode (tool=SKILL.md head,
// summary=description, inline=resource list).
func CandidateFromSkill(s skillHit) KnowledgeCandidate {
	c := KnowledgeCandidate{
		AssetID:   s.AssetID,
		AssetType:  domain.AssetTypeSkill,
		Title:     s.Title,
		Snippet:   s.Snippet,
		Score:     s.Score,
		Freshness: s.Freshness,
		Citation: Citation{
			AssetID:    s.AssetID,
			AssetType:   domain.AssetTypeSkill,
			SourceRef:  s.SourceRef,
			Locator:    s.Locator,
		},
	}
	if s.AssetVersionID != nil {
		c.AssetVersion = s.AssetVersionID
	}
	if s.VersionOrRevision != "" {
		c.Citation.VersionOrRevision = s.VersionOrRevision
	}
	return c
}

// skillHit is the narrow view of a skill DeliveryResult the SkillQuery adapter
// extracts, trimmed by the binding's delivery_mode.
type skillHit struct {
	AssetID          uuid.UUID
	AssetVersionID   *uuid.UUID
	Title            string
	Snippet          string
	Score            float64
	Freshness        float64
	SourceRef        string
	VersionOrRevision string
	Locator          map[string]any
}
