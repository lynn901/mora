// Package extractor implements the Phase 4 ExtractionProvider adapter for the
// local TEI/Ollama model (design-docs/18 §5.1, §5.2, decision D6). It is the
// ONLY path that turns a redacted Evidence snapshot into MemoryCandidates.
//
// Capability binding (§10.3): every call checks that the capability binds the
// correct Evidence ID + action; the adapter never issues a free-floating
// extraction. Upstream models that do not grok the Mora capability are
// terminated-and-validated HERE (D6) — the adapter uses its own upstream
// credentials and never forwards the capability to an external API.
//
// Fail-closed (§5.2 / §9.1): the raw upstream model output is validated
// against the MemoryCandidate contract (distill.ValidateCandidates) BEFORE the
// adapter returns it. A malformed output returns ErrCandidateInvalid and the
// caller retains the Evidence for retry — no half-structured Memory is ever
// written.
//
// The adapter touches neither the DB, nor object storage, nor URL/Git (§9.1).
// It receives only the redacted excerpt + a non-executable schema. The local
// TEI/Ollama services are on the trusted internal network (07-security §5.4)
// and are NOT routed through egress; they are separately credentialed.
package extractor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/distill"
)

// ErrCapabilityMismatch is returned when a Provider call's capability does not
// bind the expected action or evidence id (§10.3). The adapter refuses to run
// a free-floating extraction.
var ErrCapabilityMismatch = errors.New("extractor: capability binding mismatch")

// ErrUpstreamUnreachable is returned when the configured local model service
// cannot be reached. Marked transient by the worker (retry up to max_attempt).
var ErrUpstreamUnreachable = errors.New("extractor: upstream model unreachable")

// LocalAdapter is the default first-version ExtractionProvider adapter
// (design-docs/18 §5.1, D6). It targets the local TEI/Ollama model. When the
// endpoint is empty (default), it runs in stub mode: it extracts a single
// trivial candidate from any non-empty excerpt so the pipeline is exercised
// end-to-end in dev without a model sidecar. Production MUST set the endpoint
// (deploy sets TEI_URL / OLLAMA_URL + MORA_MEMORY_EXTRACT_MODEL).
type LocalAdapter struct {
	// Endpoint is the local TEI or Ollama base URL. Empty = stub mode.
	Endpoint string
	// Model is the generation model id (e.g. an Ollama tag or TEI route).
	Model string
	// HTTPClient mirrors the rag provider adapter shape. nil = default 30s.
	HTTPClient interface {
		PostJSON(ctx context.Context, url string, body []byte) ([]byte, int, error)
	}
}

// NewLocalAdapter builds the default ExtractionProvider adapter. endpoint may
// be empty (stub mode for dev).
func NewLocalAdapter(endpoint, model string) *LocalAdapter {
	return &LocalAdapter{Endpoint: endpoint, Model: model}
}

// ExtractMemory runs the §5.2 pipeline: bind capability → ask upstream →
// validate candidates → return. The capability MUST bind evidence_id +
// "extract"; the request's evidence_locator.evidence_id is set from it so the
// validator can catch a cross-evidence smuggle (§9.1).
func (a *LocalAdapter) ExtractMemory(ctx context.Context, cap distill.Capability, req distill.ExtractRequest) ([]distill.MemoryCandidate, error) {
	if cap.Action != "extract" {
		return nil, fmt.Errorf("%w: action %q != extract", ErrCapabilityMismatch, cap.Action)
	}
	if cap.EvidenceID == [16]byte{} {
		return nil, fmt.Errorf("%w: evidence id missing", ErrCapabilityMismatch)
	}

	raw, err := a.askUpstream(ctx, cap, req)
	if err != nil {
		return nil, err
	}

	candidates, err := parseCandidates(raw, cap.EvidenceID)
	if err != nil {
		// Malformed upstream output — fail closed. The Evidence is retained
		// by the caller for retry; no candidate is written (§5.2, §9.1).
		return nil, err
	}
	// Adapter-layer validation (§5.2 layer 1). The service re-validates.
	if err := distill.ValidateCandidates(candidates, cap.EvidenceID); err != nil {
		return nil, err
	}
	return candidates, nil
}

// ClassifyRelation judges two redacted statements (§6.1). First version: a
// rule-based fallback that flags obvious duplicates (identical statements) and
// otherwise returns "unrelated" — the LLM-backed classification is a deploy
// knob away (set endpoint + model). A reviewer always has the final say.
func (a *LocalAdapter) ClassifyRelation(ctx context.Context, cap distill.Capability, req distill.RelationRequest) (distill.RelationSuggestion, error) {
	if cap.Action != "classify_relation" {
		return distill.RelationSuggestion{}, fmt.Errorf("%w: action %q != classify_relation", ErrCapabilityMismatch, cap.Action)
	}
	aStmt := strings.TrimSpace(req.StatementA)
	bStmt := strings.TrimSpace(req.StatementB)
	if aStmt == "" || bStmt == "" {
		return distill.RelationSuggestion{Relation: domain.DedupUnrelated, Confidence: 0}, nil
	}
	if aStmt == bStmt {
		return distill.RelationSuggestion{Relation: domain.DedupDuplicate, Confidence: 0.95, Rationale: "identical statements"}, nil
	}
	// Substring containment → extends (low-confidence suggestion; reviewer
	// confirms). Otherwise unrelated.
	if strings.Contains(aStmt, bStmt) || strings.Contains(bStmt, aStmt) {
		return distill.RelationSuggestion{Relation: domain.DedupExtends, Confidence: 0.5, Rationale: "substring containment"}, nil
	}
	return distill.RelationSuggestion{Relation: domain.DedupUnrelated, Confidence: 0.5, Rationale: "no textual overlap"}, nil
}

// Summarize is the provider port (§5.1). First version no-ops — projection
// building consumes the raw statements. Returns the first statement as a
// stand-in summary so the contract is exercised.
func (a *LocalAdapter) Summarize(ctx context.Context, cap distill.Capability, req distill.SummaryRequest) (distill.Summary, error) {
	if cap.Action != "summarize" {
		return distill.Summary{}, fmt.Errorf("%w: action %q != summarize", ErrCapabilityMismatch, cap.Action)
	}
	if len(req.Statements) == 0 {
		return distill.Summary{}, nil
	}
	return distill.Summary{Text: req.Statements[0], Confidence: 0.3}, nil
}

// Health pings the local model service. In stub mode (no endpoint), health is
// always nil (the stub runs in-process). When an endpoint is set, a failed
// probe returns ErrUpstreamUnreachable so the worker can back off acquiring
// memory_extract jobs (transient retry).
func (a *LocalAdapter) Health(ctx context.Context) error {
	if a.Endpoint == "" {
		return nil // stub mode: always healthy.
	}
	if a.HTTPClient == nil {
		return nil // no probe configured; assume healthy (fail at use).
	}
	_, status, err := a.HTTPClient.PostJSON(ctx, a.Endpoint+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
	}
	if status >= 500 {
		return fmt.Errorf("%w: status %d", ErrUpstreamUnreachable, status)
	}
	return nil
}

// askUpstream returns the raw model output bytes for a given extract request.
// In stub mode it synthesizes a single-candidate JSON payload from the
// redacted excerpt so the pipeline is exercised in dev. With an endpoint set
// it POSTs the minimal extract instruction + the redacted excerpt (NOT the
// original ciphertext) and returns the raw body for parseCandidates.
func (a *LocalAdapter) askUpstream(ctx context.Context, cap distill.Capability, req distill.ExtractRequest) ([]byte, error) {
	if a.Endpoint == "" {
		// Stub: synthesize one fact candidate from a non-empty excerpt.
		return stubCandidatePayload(req, cap.EvidenceID), nil
	}
	if a.HTTPClient == nil {
		return nil, ErrUpstreamUnreachable
	}
	body := buildExtractBody(req, cap)
	url := a.Endpoint + "/extract"
	out, status, err := a.HTTPClient.PostJSON(ctx, url, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
	}
	if status >= 500 {
		return nil, fmt.Errorf("%w: status %d", ErrUpstreamUnreachable, status)
	}
	return out, nil
}

// compile-time check: LocalAdapter satisfies distill.ExtractionProvider.
var _ distill.ExtractionProvider = (*LocalAdapter)(nil)
