package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/distill"
)

func TestLocalAdapter_StubExtract_RoundTrip(t *testing.T) {
	t.Parallel()
	a := NewLocalAdapter("", "") // stub mode
	eid := uuid.New()
	cap := distill.Capability{Action: "extract", EvidenceID: eid, WorkspaceID: uuid.New()}
	req := distill.ExtractRequest{RedactedExcerpt: "mora-api 监听 :8990", SourceKind: domain.EvidenceSourceSession}
	cands, err := a.ExtractMemory(context.Background(), cap, req)
	if err != nil {
		t.Fatalf("ExtractMemory: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 stub candidate, got %d", len(cands))
	}
	if cands[0].EvidenceLocator.EvidenceID != eid {
		t.Fatalf("candidate evidence_id not bound: got %s", cands[0].EvidenceLocator.EvidenceID)
	}
	if cands[0].MemoryType != domain.MemoryFact {
		t.Fatalf("expected fact, got %q", cands[0].MemoryType)
	}
}

func TestLocalAdapter_RejectsWrongAction(t *testing.T) {
	t.Parallel()
	a := NewLocalAdapter("", "")
	cap := distill.Capability{Action: "classify_relation", EvidenceID: uuid.New()}
	_, err := a.ExtractMemory(context.Background(), cap, distill.ExtractRequest{})
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch, got %v", err)
	}
}

func TestLocalAdapter_FailClosedOnMalformed(t *testing.T) {
	t.Parallel()
	a := &LocalAdapter{
		Endpoint:  "http://stub",
		HTTPClient: &fakeHTTP{body: []byte(`not json at all`)},
	}
	eid := uuid.New()
	cap := distill.Capability{Action: "extract", EvidenceID: eid, WorkspaceID: uuid.New()}
	_, err := a.ExtractMemory(context.Background(), cap, distill.ExtractRequest{RedactedExcerpt: "x"})
	if err == nil {
		t.Fatal("expected error for malformed upstream output")
	}
}

func TestLocalAdapter_ParsesEnvelope(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	cand := distill.MemoryCandidate{
		MemoryType:      domain.MemoryDecision,
		Statement:       "决定 mora-api 监听 8990",
		Confidence:      0.7,
		EvidenceLocator: distill.EvidenceLocator{EvidenceID: eid},
	}
	env, _ := json.Marshal(map[string]any{"candidates": []distill.MemoryCandidate{cand}})
	got, err := parseCandidates(env, eid)
	if err != nil {
		t.Fatalf("parseCandidates envelope: %v", err)
	}
	if len(got) != 1 || got[0].EvidenceLocator.EvidenceID != eid {
		t.Fatalf("envelope parse wrong: %+v", got)
	}
}

func TestLocalAdapter_ParsesFencedJSON(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	raw := []byte("```json\n[{\"memory_type\":\"fact\",\"statement\":\"x\",\"confidence\":0.5,\"evidence_locator\":{\"evidence_id\":\"" + eid.String() + "\"}}]\n```")
	got, err := parseCandidates(raw, eid)
	if err != nil {
		t.Fatalf("parseCandidates fenced: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
}

func TestLocalAdapter_BindsMissingEvidenceID(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	// Upstream omits evidence_id; adapter must bind it.
	raw := []byte(`[{"memory_type":"fact","statement":"x","confidence":0.5}]`)
	got, err := parseCandidates(raw, eid)
	if err != nil {
		t.Fatalf("parseCandidates: %v", err)
	}
	if got[0].EvidenceLocator.EvidenceID != eid {
		t.Fatalf("adapter did not bind evidence_id: got %s", got[0].EvidenceLocator.EvidenceID)
	}
	// ValidateCandidates then passes (eid matches).
	if err := distill.ValidateCandidates(got, eid); err != nil {
		t.Fatalf("re-validate: %v", err)
	}
}

func TestLocalAdapter_CrossEvidenceSmuggleCaught(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	other := uuid.New()
	raw := []byte(`[{"memory_type":"fact","statement":"x","confidence":0.5,"evidence_locator":{"evidence_id":"` + other.String() + `"}}]`)
	_, err := parseCandidates(raw, eid)
	if err != nil {
		t.Fatalf("parse should succeed (bind only fills nil); got %v", err)
	}
	// The adapter's ValidateCandidates must catch the non-matching id.
	got, _ := parseCandidates(raw, eid)
	err = distill.ValidateCandidates(got, eid)
	if !errors.Is(err, distill.ErrCandidateInvalid) {
		t.Fatalf("expected ErrCandidateInvalid for cross-evidence, got %v", err)
	}
}

func TestLocalAdapter_ClassifyRelation(t *testing.T) {
	t.Parallel()
	a := NewLocalAdapter("", "")
	cap := distill.Capability{Action: "classify_relation", EvidenceID: uuid.New()}
	// Identical → duplicate.
	got, err := a.ClassifyRelation(context.Background(), cap, distill.RelationRequest{StatementA: "s", StatementB: "s"})
	if err != nil || got.Relation != domain.DedupDuplicate {
		t.Fatalf("identical → duplicate, got %v %v", got, err)
	}
	// Substring → extends.
	got, _ = a.ClassifyRelation(context.Background(), cap, distill.RelationRequest{StatementA: "abc def", StatementB: "abc"})
	if got.Relation != domain.DedupExtends {
		t.Fatalf("substring → extends, got %v", got.Relation)
	}
	// Disjoint → unrelated.
	got, _ = a.ClassifyRelation(context.Background(), cap, distill.RelationRequest{StatementA: "foo", StatementB: "bar"})
	if got.Relation != domain.DedupUnrelated {
		t.Fatalf("disjoint → unrelated, got %v", got.Relation)
	}
}

func TestLocalAdapter_Health(t *testing.T) {
	t.Parallel()
	// Stub mode: always healthy.
	a := NewLocalAdapter("", "")
	if err := a.Health(context.Background()); err != nil {
		t.Fatalf("stub Health: %v", err)
	}
	// Endpoint + 500 → unreachable.
	b := &LocalAdapter{Endpoint: "http://x", HTTPClient: &fakeHTTP{status: 503}}
	if err := b.Health(context.Background()); !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("expected ErrUpstreamUnreachable, got %v", err)
	}
}

// fakeHTTP satisfies the LocalAdapter.HTTPClient shape for tests.
type fakeHTTP struct {
	body   []byte
	status int
	err    error
}

func (f *fakeHTTP) PostJSON(ctx context.Context, url string, body []byte) ([]byte, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	if f.body != nil {
		return f.body, f.status, nil
	}
	return nil, f.status, nil
}
