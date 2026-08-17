// Package extractor — upstream I/O helpers (design-docs/18 §5.2, §9.1).
//
// These helpers translate between the distill.MemoryCandidate contract and the
// local model's wire shape. The adapter calls parseCandidates on the raw model
// output, then re-validates the parsed candidates (adapter layer of the §5.2
// dual-layer check). A malformed output fails closed — no half-structured
// Memory is written (§9.1).
package extractor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/distill"
)

// parseCandidates decodes the raw model output into MemoryCandidates. The
// adapter accepts two wire shapes:
//   - a JSON array of candidate objects (the canonical shape), or
//   - a JSON object with a "candidates" array (common LLM envelope).
// Any other shape returns ErrCandidateInvalid (fail closed, §5.2).
func parseCandidates(raw []byte, evidenceID uuid.UUID) ([]distill.MemoryCandidate, error) {
	raw = normalizeJSON(raw)
	if len(raw) == 0 {
		return nil, nil
	}

	// Try a bare array first.
	var arr []distill.MemoryCandidate
	if err := json.Unmarshal(raw, &arr); err == nil {
		return bindEvidenceIDs(arr, evidenceID), nil
	}

	// Fall back to an envelope object.
	var env struct {
		Candidates []distill.MemoryCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Candidates != nil {
		return bindEvidenceIDs(env.Candidates, evidenceID), nil
	}

	return nil, fmt.Errorf("%w: upstream output is not a candidate array or envelope", distill.ErrCandidateInvalid)
}

// bindEvidenceIDs fills in the evidence_locator.evidence_id when the upstream
// model omits it (capability-bound, §10.3) — every candidate is bound to the
// Evidence this extraction was scoped to. If the model emits a DIFFERENT id,
// ValidateCandidates (called next) catches it as a cross-evidence smuggle.
func bindEvidenceIDs(cs []distill.MemoryCandidate, evidenceID uuid.UUID) []distill.MemoryCandidate {
	for i := range cs {
		if cs[i].EvidenceLocator.EvidenceID == uuid.Nil {
			cs[i].EvidenceLocator.EvidenceID = evidenceID
		}
	}
	return cs
}

// normalizeJSON trims whitespace and strips a ```json``` code fence if the
// model wrapped its output (common with chat-completion models). A purely
// blank payload is normalized to nil (zero candidates, valid).
func normalizeJSON(raw []byte) []byte {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return []byte(s)
}

// stubCandidatePayload synthesizes a single-candidate JSON array for the stub
// path (no endpoint configured). It turns a non-empty redacted excerpt into a
// low-confidence fact candidate so the dev pipeline is exercised end-to-end
// without a model sidecar. Production MUST set the endpoint.
func stubCandidatePayload(req distill.ExtractRequest, evidenceID uuid.UUID) []byte {
	stmt := strings.TrimSpace(req.RedactedExcerpt)
	if stmt == "" {
		stmt = "evidence captured (stub)"
	}
	if len(stmt) > 512 {
		stmt = stmt[:512] + "…"
	}
	now := time.Now().UTC()
	c := distill.MemoryCandidate{
		MemoryType: domain.MemoryFact,
		Statement:  stmt,
		Scope:      "stub",
		Validity:   distill.Validity{ValidFrom: &now},
		Confidence: 0.3,
		EntityKeys: map[string]any{"source_kind": string(req.SourceKind)},
		EvidenceLocator: distill.EvidenceLocator{
			EvidenceID:   evidenceID,
			QuoteLocator: map[string]any{"stub": true},
		},
	}
	out, _ := json.Marshal([]distill.MemoryCandidate{c})
	return out
}

// buildExtractBody assembles the minimal HTTP body for the local model. It
// sends ONLY the redacted excerpt + the schema-ish instruction — never the
// original ciphertext, never DB handles, never storage keys (§9.1).
func buildExtractBody(req distill.ExtractRequest, cap distill.Capability) []byte {
	body := map[string]any{
		"evidence_id":   cap.EvidenceID,
		"workspace_id":  cap.WorkspaceID,
		"redacted":      req.RedactedExcerpt,
		"source_kind":   string(req.SourceKind),
		"owner_type":    string(req.OwnerType),
		"schema":        "https://mora/phase4/memory_candidate.schema.json",
		"instruction":   "Return a JSON array of MemoryCandidate objects bound to evidence_id " + cap.EvidenceID.String() + ". memory_type ∈ fact|decision|constraint|preference|event. confidence ∈ [0,1].",
	}
	b, _ := json.Marshal(body)
	return b
}
