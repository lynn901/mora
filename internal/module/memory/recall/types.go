// Package recall implements the structured memory recall + feedback surface
// (design-docs/18 §8 召回与反馈，decision D8; doc 12 §9.3/§9.4/§9.5).
//
// Recall is the read entry point the MCP `memory_recall` tool and the REST
// GET /api/v1/memory/units endpoint call. It returns the standard
// KnowledgeCandidate (12 §9.3) carrying the statement, authority/freshness/
// confidence scores, relations (incl. contradicts), and a traceable evidence
// reference — never Provider credentials or storage addresses (§8.1).
//
// Authority policy (§9.5): under the "决策原因" intent, reviewed Memory +
// evidence are the primary basis; low-confidence or superseded memories must
// surface their conflicts — Recall never silently picks one answer (11 §6.4).
//
// Leak-safe (§9.3): unauthorized / unpublished / private-but-not-owner rows
// never surface — the caller gets an empty result, indistinguishable from a
// 403/404, so existence is never leaked. Private candidates are readable only
// by the owner on an explicit request or in the review view (§8.5).
//
// Feedback (§8.3): useful/incorrect/stale lands in memory_feedback and adjusts
// authority/freshness + may trigger an `evidence.revalidate` outbox event. It
// never edits the statement (§8.5).
package recall

import (
	"time"

	"github.com/google/uuid"
)

// KnowledgeQuery is the unified recall input (doc 12 §9.1, narrowed for the
// memory surface). Filters narrow the workspace / owner / memory_type / time /
// validity / linked-asset axes (§8.1). IncludeCandidates opts into the
// owner-only / review-view read of unpublished private candidates (§8.5) — it
// is ONLY honored for the owner (the service rejects it for anyone else).
type KnowledgeQuery struct {
	Query    string // free-text; drives FTS + (future) vector recall over statement
	WorkspaceID uuid.UUID
	// OwnerID narrows to a single evidence owner (nil = all visible). Used by
	// the owner-only candidate read (§8.5) and the review inbox view.
	OwnerID *uuid.UUID
	// MemoryType narrows to a single distilled kind (fact/decision/…); nil = all.
	MemoryType *string
	// ValidAt filters to units valid at this instant (valid_from ≤ t ≤ expires_at
	// OR expires_at IS NULL). nil = no time filter (current).
	ValidAt *time.Time
	// AssetID narrows to units linked to a specific knowledge_asset (§8.1
	// "关联资产" filter); nil = no asset filter.
	AssetID *uuid.UUID
	// IncludeCandidates opts into unpublished (candidate/approved) units. This
	// is the §8.5 owner-only / review-view read — the service honors it ONLY
	// for the owner; a non-owner request is downgraded to published-only so a
	// private candidate's existence never leaks (§9.3).
	IncludeCandidates bool
	MaxItems int
}

// RelationSummary is one side of a relation a candidate participates in (12
// §9.3). Memory recall surfaces contradicts so conflicts are not silently
// chosen (§8.2 / 11 §6.4). RelationType mirrors knowledge_relations + the
// evidence_link support_type for the memory surface.
type RelationSummary struct {
	RelationType string // supersedes|contradicts|derived_from|related_to|supports
	TargetID     uuid.UUID
	TargetTitle  string
}

// Citation is the traceable reference a candidate carries (12 §9.3). For a
// memory candidate it points at the knowledge_asset (memory) + the backing
// evidence id + its non-executable quote locator. It never carries Provider
// credentials or storage addresses (§8.1 ProjectionRef is internal-only).
type Citation struct {
	AssetID        uuid.UUID
	AssetVersionID *uuid.UUID
	// EvidenceID is the backing memory_evidence row this statement was
	// distilled from (§8.1 — "召回结果携带可回溯证据引用"). Empty when the
	// unit has no surviving evidence link (evidence_missing).
	EvidenceID    uuid.UUID
	QuoteLocator  map[string]any
	SupportType   string // supports|contradicts
	// EvidenceMissing reflects the unit's evidence_missing flag so the caller
	// can down-weight / flag a candidate whose backing evidence is gone (§8.4).
	EvidenceMissing bool
}

// KnowledgeCandidate is the standard recall result (doc 12 §9.3). Score is the
// blended authority/freshness/confidence ranking (§9.5); Authority/Freshness/
// Confidence are the individual signals. Relations carries contradicts so
// conflicts surface (§8.2). ProjectionRef is internal-diagnostic only and is
// NOT returned across the MCP surface (§8.1).
type KnowledgeCandidate struct {
	UnitID      uuid.UUID
	AssetID     uuid.UUID
	AssetType   string // always "memory" for this surface
	MemoryType  string
	Title       string
	Snippet     string // the statement (redacted; §8.1)
	Score       float64
	Authority   float64
	Freshness   float64
	Confidence  *float64
	ContentHash string
	Relations   []RelationSummary
	Citation    Citation
	// State lets the owner/review view distinguish published vs candidate
	// results; default recall only returns published so this is "published".
	State string
	// ProjectionRef is internal diagnostic only; never serialized to the Agent
	// (§8.1 — not Provider creds/storage addresses). Kept unexported so it
	// cannot leak through JSON marshaling.
	projectionRef string
}
