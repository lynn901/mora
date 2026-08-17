// Package distill is the Phase 4 extraction pipeline (design-docs/18 §5,
// decisions D5/D6). It owns the ExtractionProvider port, the MemoryCandidate
// contract (a JSON-Schema-constrained candidate the Provider returns), the
// dual-layer validation that fail-closes on a malformed candidate, and the
// candidate inbox write path (Provider output → memory_units candidate +
// memory_evidence_links).
//
// The Provider is the ONLY path that turns an Evidence row into a structured
// Memory. Its output is validated twice (adapter + service, §5.2); a
// validation failure leaves the Evidence in place for retry and NEVER writes
// a half-structured Memory (fail closed, §9.1).
//
// The Provider touches neither the DB, nor object storage, nor URL/Git
// (§9.1). It receives only the redacted Evidence snapshot + a non-executable
// schema. Any upstream model that does not grok the Mora `capability` is
// terminated-and-validated at the Mora adapter (D6) using its own upstream
// credentials, behind the egress policy + a redacted `external_call` audit.
package distill

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// Capability is the scoped capability envelope bound to an extraction action
// (12 §10.3). `capability` MUST bind the Evidence ID + the `extract` action so
// a Provider call is never free-floating. The adapter that terminates an
// upstream model checks that the capability is honored (e.g. no cross-evidence
// reads, no DB/storage/URL/Git access, §9.1).
type Capability struct {
	Action     string    // "extract" | "classify_relation" | "summarize"
	EvidenceID uuid.UUID // the evidence this extraction is bound to (§10.3)
	WorkspaceID uuid.UUID
	// DataClassCap is the max data tier the Provider may receive (D6). A
	// secret/credential-classified Evidence is never handed to the Provider
	// — the capture gate rejects it first (§4.1); this is the backstop.
	DataClassCap domain.EvidenceClassification
}

// ExtractRequest is the redacted, minimal snapshot handed to
// ExtractMemory (§5.2). It carries the redacted excerpt (NOT the original
// ciphertext), the source kind, and the owner scope. No DB handles, no
// storage keys, no URLs — the Provider cannot reach the system.
type ExtractRequest struct {
	// RedactedExcerpt is the §4.3 redacted fragment (post-gate). The Provider
	// parses it into MemoryCandidates. It is NOT spliced into a prompt as-is;
	// the adapter wraps it in the minimal instruction needed (§9.1).
	RedactedExcerpt string
	SourceKind      domain.EvidenceSourceKind
	OwnerType        domain.OwnerType
	OwnerID          uuid.UUID
	// SourceRef is the non-executable source locator (session/message/tool_call
	// /asset_version id) for the candidate's evidence_locator.
	SourceRef string
}

// MemoryCandidate is a JSON-Schema-constrained extraction candidate (§5.2). A
// Provider returns zero or more of these per Evidence; the service validates
// each against the schema (§5.2 dual-layer) before persisting a
// memory_units(row, state=candidate) + memory_evidence_links.
//
// Fields map to the schema.json contract:
//   - memory_type ∈ {fact,decision,constraint,preference,event}
//   - statement: natural-language conclusion (already redacted)
//   - scope: applicability range (non-executable)
//   - validity.valid_from / expires_at (RFC3339, nullable)
//   - confidence ∈ [0,1]
//   - entity_keys: structured keys for exact recall (structured_payload)
//   - evidence_locator: {evidence_id, quote_locator}
type MemoryCandidate struct {
	MemoryType       domain.MemoryType `json:"memory_type"`
	Statement        string             `json:"statement"`
	Scope            string             `json:"scope,omitempty"`
	Validity         Validity           `json:"validity"`
	Confidence       float64            `json:"confidence"`
	EntityKeys       map[string]any     `json:"entity_keys,omitempty"`
	EvidenceLocator  EvidenceLocator    `json:"evidence_locator"`
}

// Validity is the time-bounding of a candidate (§5.2 schema). ExpiresAt may be
// null (open-ended fact). ValidFrom may be null (unknown start).
type Validity struct {
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// EvidenceLocator is the non-executable reference back to the source Evidence
// (§5.2). QuoteLocator carries offset/range/hash, never the original text.
type EvidenceLocator struct {
	EvidenceID   uuid.UUID      `json:"evidence_id"`
	QuoteLocator map[string]any `json:"quote_locator,omitempty"`
}

// RelationRequest is the input to ClassifyRelation (§6.1): two candidate
// statements the Provider judges as duplicate/extends/contradicts/unrelated.
// The Provider sees only the two redacted statements + their scopes — no
// Evidence IDs leak to an upstream model (the adapter binds capability).
type RelationRequest struct {
	StatementA string
	ScopeA     string
	StatementB string
	ScopeB     string
}

// RelationSuggestion is a non-merging dedup/conflict proposal (D7). Only a
// reviewer disposition ever writes superseded_by / knowledge_relations.
type RelationSuggestion struct {
	Relation   domain.DedupSuggestionType // duplicate|extends|contradicts|unrelated
	Confidence float64
	Rationale  string // short, redacted
}

// SummaryRequest is the input to Summarize (provider port, §5.1). Reserved
// for the projection-building path; Phase 4 first version may return a no-op.
type SummaryRequest struct {
	Statements []string
}

// Summary is the output of Summarize.
type Summary struct {
	Text       string
	Confidence float64
}

// ExtractionProvider is the §5.1 port (12 §10.3). The default first-version
// adapter is local TEI/Ollama (D6); capability binding is enforced at the
// adapter. The Provider MUST return JSON-Schema-constrained candidates; a
// malformed output is dropped and the Evidence retained for retry (§5.2).
type ExtractionProvider interface {
	// ExtractMemory turns a redacted Evidence snapshot into zero or more
	// MemoryCandidates. The capability binds the evidence_id + "extract".
	ExtractMemory(ctx context.Context, cap Capability, req ExtractRequest) ([]MemoryCandidate, error)
	// ClassifyRelation judges two candidate statements (§6.1 dedup). The
	// capability binds the evidence_id of one of the pair + "classify_relation".
	ClassifyRelation(ctx context.Context, cap Capability, req RelationRequest) (RelationSuggestion, error)
	// Summarize condenses a set of statements (provider port, §5.1). First
	// version may no-op.
	Summarize(ctx context.Context, cap Capability, req SummaryRequest) (Summary, error)
	// Health pings the upstream model service; nil = healthy. Used by the
	// worker readiness check before acquiring memory_extract jobs.
	Health(ctx context.Context) error
}

// CandidateWriter is the persistence port the distill service uses to land
// validated candidates (§5.3). It is a narrow slice of evidence.MemoryUnitRepo
// + EvidenceLinkRepo so the service composes them without owning the full
// CRUD surface. Writes are transactional with the outbox event.
type CandidateWriter interface {
	// InsertUnit persists a memory_units row in the candidate state. The
	// caller has already validated the candidate against the schema (§5.2).
	InsertUnit(ctx context.Context, u domain.MemoryUnit) (uuid.UUID, error)
	// LinkEvidence records a memory_evidence_links row (unit ↔ evidence,
	// quote_locator, support_type).
	LinkEvidence(ctx context.Context, l domain.MemoryEvidenceLink) error
}

// candidateWriter composes the two repos into the CandidateWriter port.
type candidateWriter struct {
	units  evidence.MemoryUnitRepo
	links  evidence.EvidenceLinkRepo
}

// NewCandidateWriter composes a MemoryUnitRepo + EvidenceLinkRepo into the
// distill CandidateWriter port.
func NewCandidateWriter(units evidence.MemoryUnitRepo, links evidence.EvidenceLinkRepo) CandidateWriter {
	return &candidateWriter{units: units, links: links}
}

func (w *candidateWriter) InsertUnit(ctx context.Context, u domain.MemoryUnit) (uuid.UUID, error) {
	return w.units.Insert(ctx, u)
}

func (w *candidateWriter) LinkEvidence(ctx context.Context, l domain.MemoryEvidenceLink) error {
	return w.links.Insert(ctx, l)
}
