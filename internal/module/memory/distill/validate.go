// Package distill — candidate validation (design-docs/18 §5.2, §9.1).
//
// The Provider's output is validated against the MemoryCandidate contract
// (schema.json) at TWO layers (§5.2):
//   1. Adapter layer — the Provider adapter validates the raw upstream model
//      output before returning it from ExtractMemory (so a malformed upstream
//      response never escapes the adapter).
//   2. Service layer — the distill service re-validates the candidates it
//      receives before persisting a memory_units row (defense in depth: a
//      buggy/compromised adapter cannot land a half-structured Memory).
//
// A validation failure at either layer returns an ErrCandidateInvalid and the
// Evidence is retained for retry — NEVER write a half-structured Memory
// (fail closed, §9.1). The validator here is the single source of truth used
// by both layers (they call the same ValidateCandidates), so the two layers
// cannot drift apart.
//
// The validator is a hand-rolled structural check against the schema.json
// contract. It deliberately does not pull a JSON-Schema library: the contract
// is small and fixed (5 memory types, 5 statement bounds, uuid evidence_id,
// confidence ∈ [0,1]), the runtime is offline-first (no network to fetch a
// $schema), and a self-contained check keeps the failure mode auditable.
package distill

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// ErrCandidateInvalid is the fail-closed outcome for a candidate that does not
// satisfy the schema (§5.2). The caller drops the candidate and retains the
// Evidence for retry; no memory_units row is written.
var ErrCandidateInvalid = errors.New("memory: candidate failed schema validation")

// maxStatementLen bounds the natural-language conclusion (schema.json
// statement.maxLength). A longer statement is rejected — the Provider must
// summarize, not dump the source.
const maxStatementLen = 4096

// maxScopeLen bounds the non-executable applicability range.
const maxScopeLen = 1024

// ValidateCandidates runs the §5.2 contract check over a slice of
// MemoryCandidate. It returns the first error encountered (fail fast) and is
// called by both the adapter and the service so the two validation layers
// share one truth. A nil slice / empty slice is valid — a Provider may
// legitimately extract zero candidates from an uninformative Evidence.
func ValidateCandidates(candidates []MemoryCandidate, boundEvidenceID uuid.UUID) error {
	for i, c := range candidates {
		if err := validateOne(c, boundEvidenceID); err != nil {
			return fmt.Errorf("%w: candidate[%d]: %s", ErrCandidateInvalid, i, err)
		}
	}
	return nil
}

// validateOne checks a single candidate against the schema contract. The
// boundEvidenceID is the capability-bound Evidence ID (§10.3) — every
// candidate's evidence_locator.evidence_id MUST equal it, so a Provider cannot
// smuggle a cross-evidence reference (§9.1).
func validateOne(c MemoryCandidate, boundEvidenceID uuid.UUID) error {
	switch c.MemoryType {
	case domain.MemoryFact, domain.MemoryDecision, domain.MemoryConstraint,
		domain.MemoryPreference, domain.MemoryEvent:
	default:
		return fmt.Errorf("memory_type %q not in enum", c.MemoryType)
	}
	stmt := strings.TrimSpace(c.Statement)
	if stmt == "" {
		return errors.New("statement must not be empty")
	}
	if len(stmt) > maxStatementLen {
		return fmt.Errorf("statement length %d exceeds %d", len(stmt), maxStatementLen)
	}
	if len(c.Scope) > maxScopeLen {
		return fmt.Errorf("scope length %d exceeds %d", len(c.Scope), maxScopeLen)
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return fmt.Errorf("confidence %v out of [0,1]", c.Confidence)
	}
	// Validity: formats + ordering (expires_at ≥ valid_from when both set).
	if c.Validity.ValidFrom != nil {
		if _, err := time.Parse(time.RFC3339Nano, c.Validity.ValidFrom.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("valid_from not a valid time: %w", err)
		}
	}
	if c.Validity.ExpiresAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, c.Validity.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("expires_at not a valid time: %w", err)
		}
	}
	if c.Validity.ValidFrom != nil && c.Validity.ExpiresAt != nil {
		if c.Validity.ExpiresAt.Before(*c.Validity.ValidFrom) {
			return errors.New("expires_at before valid_from")
		}
	}
	// evidence_locator.evidence_id MUST equal the capability-bound Evidence ID
	// (§10.3 capability binding — no cross-evidence smuggle, §9.1).
	if c.EvidenceLocator.EvidenceID == uuid.Nil {
		return errors.New("evidence_locator.evidence_id missing")
	}
	if c.EvidenceLocator.EvidenceID != boundEvidenceID {
		return fmt.Errorf("evidence_locator.evidence_id %s != capability-bound %s",
			c.EvidenceLocator.EvidenceID, boundEvidenceID)
	}
	return nil
}
