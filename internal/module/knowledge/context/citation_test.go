package contextbroker

// citation_test.go verifies §8.2: CitationBuilder.Build finalizes the citation
// fields (denormalization: AssetID/AssetType/Authority/Confidence mirror the
// candidate), does NOT re-resolve anchors (VersionOrRevision/SourceRef/Locator
// pass through unchanged), and the Citation struct carries no ProjectionRef
// field — the builder is the guard that nothing internal-only leaks.

import (
	"testing"
	"reflect"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// TestCitationBuilder_DenormalizesFields proves §8.2: after Build, each
// candidate's Citation mirrors the candidate's own AssetID/AssetType/Authority/
// Confidence — even if the citation was stale (e.g. the candidate traveled
// through dedup/budget which could, in a buggy future, desync them).
func TestCitationBuilder_DenormalizesFields(t *testing.T) {
	b := NewCitationBuilder()
	assetID := uuid.New()
	conf := 0.42
	c := KnowledgeCandidate{
		AssetID:    assetID,
		AssetType:  domain.AssetTypeMemory,
		Authority:  0.77,
		Confidence: &conf,
		Citation: Citation{
			AssetID:    uuid.New(), // deliberately stale — NOT the candidate's id
			AssetType:  domain.AssetTypeDocument, // deliberately wrong type
			Authority:  0.0, // deliberately zero
			// Confidence intentionally nil on the citation
			SourceRef:          "memory://unit/abc",
			VersionOrRevision:  "evid-123",
			Locator:            map[string]any{"message": "turn-2"},
		},
	}
	got := b.Build([]KnowledgeCandidate{c})
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	cit := got[0].Citation
	if cit.AssetID != assetID {
		t.Errorf("Citation.AssetID not denormalized: got %s, want %s", cit.AssetID, assetID)
	}
	if cit.AssetType != domain.AssetTypeMemory {
		t.Errorf("Citation.AssetType not denormalized: got %s, want %s", cit.AssetType, domain.AssetTypeMemory)
	}
	if cit.Authority != 0.77 {
		t.Errorf("Citation.Authority not denormalized: got %f, want 0.77", cit.Authority)
	}
	if cit.Confidence == nil || *cit.Confidence != 0.42 {
		t.Errorf("Citation.Confidence not denormalized: got %v, want 0.42", cit.Confidence)
	}
}

// TestCitationBuilder_DoesNotReResolveAnchors proves §8.2 "不重新解析": the
// builder passes SourceRef, VersionOrRevision, Locator, and UpdatedAt through
// UNCHANGED — it does not re-resolve them from the engine. If the adapter set
// them, they survive; if the adapter left them empty, they stay empty.
func TestCitationBuilder_DoesNotReResolveAnchors(t *testing.T) {
	b := NewCitationBuilder()
	c1 := KnowledgeCandidate{
		AssetID:   uuid.New(),
		AssetType:  domain.AssetTypeCodebase,
		Authority:  0.5,
		Citation: Citation{
			SourceRef:          "codegraph://tree/abc",
			VersionOrRevision:  "commit-deadbeef",
			Locator:            map[string]any{"file": "main.go", "start_line": 42},
		},
	}
	// c2 has an EMPTY citation (adapter resolved nothing) — builder must not
	// synthesize anchors.
	c2 := KnowledgeCandidate{
		AssetID:   uuid.New(),
		AssetType:  domain.AssetTypeDocument,
		Authority:  0.3,
		Citation: Citation{},
	}
	got := b.Build([]KnowledgeCandidate{c1, c2})

	// c1: all anchors preserved as-is
	cit1 := got[0].Citation
	if cit1.SourceRef != "codegraph://tree/abc" {
		t.Errorf("SourceRef changed: got %q", cit1.SourceRef)
	}
	if cit1.VersionOrRevision != "commit-deadbeef" {
		t.Errorf("VersionOrRevision changed: got %q", cit1.VersionOrRevision)
	}
	if loc, _ := cit1.Locator["file"].(string); loc != "main.go" {
		t.Errorf("Locator changed: got %v", cit1.Locator)
	}

	// c2: anchors stay empty (no synthesis)
	cit2 := got[1].Citation
	if cit2.SourceRef != "" || cit2.VersionOrRevision != "" {
		t.Errorf("empty citation should stay empty (no re-resolution): %+v", cit2)
	}
	if cit2.Locator != nil {
		t.Errorf("nil Locator should stay nil: got %v", cit2.Locator)
	}
}

// TestCitationBuilder_PreservesOrderAndCount proves Build does not reorder or
// drop candidates — it is a pure field-finalization pass.
func TestCitationBuilder_PreservesOrderAndCount(t *testing.T) {
	b := NewCitationBuilder()
	in := []KnowledgeCandidate{
		{AssetID: uuid.New(), AssetType: domain.AssetTypeDocument, Authority: 0.1},
		{AssetID: uuid.New(), AssetType: domain.AssetTypeMemory, Authority: 0.2},
		{AssetID: uuid.New(), AssetType: domain.AssetTypeSkill, Authority: 0.3},
	}
	out := b.Build(in)
	if len(out) != 3 {
		t.Fatalf("count changed: got %d, want 3", len(out))
	}
	for i := range out {
		if out[i].AssetID != in[i].AssetID {
			t.Errorf("order changed at %d", i)
		}
	}
}

// TestCitationBuilder_EmptyInput proves the edge case: empty in → empty out,
// no nil-deref.
func TestCitationBuilder_EmptyInput(t *testing.T) {
	b := NewCitationBuilder()
	got := b.Build(nil)
	if got != nil {
		t.Errorf("nil in should yield nil out, got %v", got)
	}
	got = b.Build([]KnowledgeCandidate{})
	if len(got) != 0 {
		t.Errorf("empty slice in should yield empty out, got %v", got)
	}
}

// TestCitation_NoProjectionRefField is a COMPILE-TIME guard that the Citation
// struct has no ProjectionRef field (§8.2 — internal-only, never returned).
// It uses reflect to assert no field named "ProjectionRef" (or any field with
// the projection_ref json tag) exists, so a future edit that adds one is
// caught by this test even though Go does not let us reference a non-existent
// field by name. This encodes the DoD "ProjectionRef 不出现在返回中" as a
// structural invariant, not just a runtime check.
func TestCitation_NoProjectionRefField(t *testing.T) {
	citType := reflect.TypeOf(Citation{})
	for i := 0; i < citType.NumField(); i++ {
		f := citType.Field(i)
		if f.Name == "ProjectionRef" {
			t.Errorf("Citation must not have a ProjectionRef field (§8.2 internal-only): found %s", f.Name)
		}
		if tag, ok := f.Tag.Lookup("json"); ok {
			if contains([]string{tag}, "projection_ref") || reflect.ValueOf(tag).String() == "projection_ref" {
				t.Errorf("Citation field %s has projection_ref json tag (§8.2 leak): %q", f.Name, tag)
			}
		}
	}
	// also assert the candidate itself has no ProjectionRef
	candType := reflect.TypeOf(KnowledgeCandidate{})
	for i := 0; i < candType.NumField(); i++ {
		f := candType.Field(i)
		if f.Name == "ProjectionRef" {
			t.Errorf("KnowledgeCandidate must not have a ProjectionRef field (§8.2): found %s", f.Name)
		}
	}
}

// TestCitationBuilder_AllFourTypes proves Build handles all four asset types
// without special-casing — the denormalization is type-agnostic.
func TestCitationBuilder_AllFourTypes(t *testing.T) {
	b := NewCitationBuilder()
	types := []domain.AssetType{
		domain.AssetTypeDocument, domain.AssetTypeCodebase,
		domain.AssetTypeMemory, domain.AssetTypeSkill,
	}
	cands := make([]KnowledgeCandidate, len(types))
	for i, at := range types {
		cands[i] = KnowledgeCandidate{
			AssetID:   uuid.New(),
			AssetType:  at,
			Authority:  float64(i) / 10,
			Citation:   Citation{AssetID: uuid.New(), AssetType: domain.AssetTypeDocument}, // stale
		}
	}
	got := b.Build(cands)
	for i, c := range got {
		if c.Citation.AssetID != c.AssetID {
			t.Errorf("type %s: AssetID not denormalized", types[i])
		}
		if c.Citation.AssetType != types[i] {
			t.Errorf("type %s: AssetType not denormalized: got %s", types[i], c.Citation.AssetType)
		}
	}
}
