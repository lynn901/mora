package context

// citation_test.go verifies CitationBuilder.Build (YS-208 DoD "CitationBuilder.
// Build 补齐引用字段，ProjectionRef 不出现在返回中"). It pins:
//   - Post-authz field completion: source_ref / version_or_revision /
//     updated_at / locator filled from CitationMetaLookup (§8.2).
//   - Reuse, no re-parse: adapter-supplied locator keys (block_id, file:line,
//     quote) survive — the Builder merges, never drops carried keys (§8.2).
//   - ProjectionRef is not on the Citation struct (§8.2 — internal-only); the
//     returned citation cannot carry it.
//   - Nil lookup → pure field-map from the candidate's carried citation.
//   - Self-contained identity denormalization (AssetID/AssetType/Authority,
//     §9.3).
//   - Post-authz by construction: only the asset IDs the Broker passes are
//     resolved (the lookup cannot leak an unauthorized asset).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

func TestCitationBuilder_PostAuthzCompletesFields(t *testing.T) {
	// §8.2: the lookup completes source_ref, version_or_revision, updated_at,
	// and locator. The candidate arrives with an adapter-supplied locator;
	// the lookup ADDS keys without dropping the carried ones.
	assetID := id("doc")
	cand := KnowledgeCandidate{
		AssetID:   assetID,
		AssetType:  domain.AssetTypeDocument,
		Authority:  0.9,
		Citation: Citation{
			AssetID:   assetID,
			AssetType:  domain.AssetTypeDocument,
			Locator:   map[string]any{"block_id": 12}, // adapter-supplied
		},
	}
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lookup := &fakeMetaLookup{
		meta: map[uuid.UUID]CitationMeta{
			assetID: {
				AssetID:           assetID,
				AssetType:          domain.AssetTypeDocument,
				SourceRef:         "mora/search",
				VersionOrRevision: "v3",
				UpdatedAt:         updated,
				Locator:           map[string]any{"section_path": "§2"}, // added by lookup
			},
		},
	}
	b := NewCitationBuilder(lookup)
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
	c := out[0].Citation
	if c.SourceRef != "mora/search" {
		t.Errorf("SourceRef = %q, want mora/search", c.SourceRef)
	}
	if c.VersionOrRevision != "v3" {
		t.Errorf("VersionOrRevision = %q, want v3", c.VersionOrRevision)
	}
	if !c.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt = %v, want %v", c.UpdatedAt, updated)
	}
	// No re-parse: carried block_id survives; lookup's section_path added.
	if c.Locator["block_id"] != 12 {
		t.Errorf("carried block_id dropped: %v", c.Locator)
	}
	if c.Locator["section_path"] != "§2" {
		t.Errorf("lookup section_path not added: %v", c.Locator)
	}
}

func TestCitationBuilder_DenormalizedIdentitySelfContained(t *testing.T) {
	// §9.3: the citation is self-contained for audit — AssetID/AssetType/
	// Authority/Confidence denormalized from the candidate.
	assetID := id("mem")
	conf := 0.8
	cand := KnowledgeCandidate{
		AssetID:    assetID,
		AssetType:   domain.AssetTypeMemory,
		Authority:  0.7,
		Confidence: &conf,
	}
	b := NewCitationBuilder(nil) // nil lookup → pure field-map
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	c := out[0].Citation
	if c.AssetID != assetID || c.AssetType != domain.AssetTypeMemory {
		t.Errorf("identity not denormalized: %+v", c)
	}
	if c.Authority != 0.7 {
		t.Errorf("Authority not denormalized: %v", c.Authority)
	}
	if c.Confidence == nil || *c.Confidence != conf {
		t.Errorf("Confidence not denormalized: %v", c.Confidence)
	}
}

func TestCitationBuilder_NilLookupKeepsCarriedCitation(t *testing.T) {
	// §8.2: nil lookup → the Builder does no IO; it keeps the adapter-supplied
	// source_ref/version_or_revision/locator as-is (no re-parse, no drop).
	assetID := id("code")
	cand := KnowledgeCandidate{
		AssetID:   assetID,
		AssetType:  domain.AssetTypeCodebase,
		Citation: Citation{
			AssetID:           assetID,
			AssetType:          domain.AssetTypeCodebase,
			SourceRef:         "refs/heads/main",
			VersionOrRevision: "abc1234",
			Locator:           map[string]any{"file": "a.go", "start_line": 10},
		},
	}
	b := NewCitationBuilder(nil)
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	c := out[0].Citation
	if c.SourceRef != "refs/heads/main" || c.VersionOrRevision != "abc1234" {
		t.Errorf("carried citation fields changed: %+v", c)
	}
	if c.Locator["file"] != "a.go" || c.Locator["start_line"] != 10 {
		t.Errorf("carried locator changed: %v", c.Locator)
	}
}

func TestCitationBuilder_MissingMetaLeavesFieldsUnset(t *testing.T) {
	// §8.1: an asset with no resolvable version (lookup returns nothing) keeps
	// its adapter-supplied citation; the post-authz fields stay unset rather
	// than invented.
	assetID := id("unversioned")
	cand := KnowledgeCandidate{
		AssetID:   assetID,
		AssetType:  domain.AssetTypeMemory,
		Citation: Citation{
			AssetID:  assetID,
			Locator:  map[string]any{"quote": "..."},
		},
	}
	lookup := &fakeMetaLookup{meta: map[uuid.UUID]CitationMeta{}} // no meta for this asset
	b := NewCitationBuilder(lookup)
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	c := out[0].Citation
	if c.SourceRef != "" {
		t.Errorf("unresolved SourceRef invented: %q", c.SourceRef)
	}
	if c.VersionOrRevision != "" {
		t.Errorf("unresolved VersionOrRevision invented: %q", c.VersionOrRevision)
	}
	if !c.UpdatedAt.IsZero() {
		t.Errorf("unresolved UpdatedAt invented: %v", c.UpdatedAt)
	}
	// Carried locator survives.
	if c.Locator["quote"] != "..." {
		t.Errorf("carried quote dropped: %v", c.Locator)
	}
}

func TestCitationBuilder_LocatorMergeLookupWinsOnConflict(t *testing.T) {
	// §8.2: locator merge — lookup wins on conflict, but carried keys the
	// lookup did not set are preserved. block_id set by both → lookup value;
	// quote set only by carried → preserved.
	assetID := id("merge")
	cand := KnowledgeCandidate{
		AssetID:  assetID,
		AssetType: domain.AssetTypeMemory,
		Citation: Citation{
			AssetID: assetID,
			Locator: map[string]any{"block_id": 1, "quote": "carried"},
		},
	}
	lookup := &fakeMetaLookup{
		meta: map[uuid.UUID]CitationMeta{
			assetID: {AssetID: assetID, Locator: map[string]any{"block_id": 99, "added": "x"}},
		},
	}
	b := NewCitationBuilder(lookup)
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	loc := out[0].Citation.Locator
	if loc["block_id"] != 99 {
		t.Errorf("lookup did not win on conflict: block_id = %v, want 99", loc["block_id"])
	}
	if loc["quote"] != "carried" {
		t.Errorf("carried-only key dropped: quote = %v, want carried", loc["quote"])
	}
	if loc["added"] != "x" {
		t.Errorf("lookup-only key dropped: added = %v", loc["added"])
	}
}

func TestCitationBuilder_ProjectionRefNotPresent(t *testing.T) {
	// §8.2 / DoD: ProjectionRef is internal-diagnostic only and NOT returned.
	// The Citation struct has no ProjectionRef field, so the Builder cannot add
	// it. Verify the marshaled citation has no projection_ref key (defense
	// against a future field being added without the §8.2 guard).
	assetID := id("no_leak")
	cand := KnowledgeCandidate{
		AssetID:   assetID,
		AssetType:  domain.AssetTypeDocument,
		Citation: Citation{AssetID: assetID, AssetType: domain.AssetTypeDocument},
	}
	b := NewCitationBuilder(&fakeMetaLookup{
		meta: map[uuid.UUID]CitationMeta{
			assetID: {AssetID: assetID, SourceRef: "src", VersionOrRevision: "v1"},
		},
	})
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	// Marshal the citation and assert no projection_ref key.
	bs, err := json.Marshal(out[0].Citation)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bs), "projection_ref") {
		t.Errorf("ProjectionRef leaked into citation JSON: %s", string(bs))
	}
}

func TestCitationBuilder_PreservesOrder(t *testing.T) {
	// §8.2: Build preserves the input order (the Broker's policy+budget order).
	cands := []KnowledgeCandidate{
		{AssetID: id("a"), AssetType: domain.AssetTypeDocument, Citation: Citation{AssetID: id("a")}},
		{AssetID: id("b"), AssetType: domain.AssetTypeMemory, Citation: Citation{AssetID: id("b")}},
		{AssetID: id("c"), AssetType: domain.AssetTypeSkill, Citation: Citation{AssetID: id("c")}},
	}
	b := NewCitationBuilder(nil)
	out := b.Build(context.Background(), cands)
	if len(out) != 3 {
		t.Fatalf("got %d, want 3", len(out))
	}
	if out[0].AssetID != id("a") || out[1].AssetID != id("b") || out[2].AssetID != id("c") {
		t.Errorf("order not preserved: %v %v %v", out[0].AssetID, out[1].AssetID, out[2].AssetID)
	}
}

func TestCitationBuilder_EmptyInput(t *testing.T) {
	b := NewCitationBuilder(nil)
	out := b.Build(context.Background(), nil)
	if len(out) != 0 {
		t.Errorf("got %d, want 0", len(out))
	}
}

func TestCitationBuilder_LookupErrorDegradesToCarried(t *testing.T) {
	// §8.2: a lookup error does NOT fail Build — the Builder degrades to the
	// carried citation (partial response, not a hard error). The Broker logs
	// the degraded citation; the Agent still gets a result.
	assetID := id("degraded")
	cand := KnowledgeCandidate{
		AssetID:   assetID,
		AssetType:  domain.AssetTypeDocument,
		Citation: Citation{
			AssetID: assetID, AssetType: domain.AssetTypeDocument,
			SourceRef: "carried", VersionOrRevision: "carried-v",
		},
	}
	lookup := &fakeMetaLookup{err: errFakeLookup}
	b := NewCitationBuilder(lookup)
	out := b.Build(context.Background(), []KnowledgeCandidate{cand})
	c := out[0].Citation
	if c.SourceRef != "carried" || c.VersionOrRevision != "carried-v" {
		t.Errorf("lookup error did not degrade to carried: %+v", c)
	}
}

// fakeMetaLookup is a CitationMetaLookup fake.
type fakeMetaLookup struct {
	meta map[uuid.UUID]CitationMeta
	err  error
}

func (f *fakeMetaLookup) Lookup(ctx context.Context, workspaceID uuid.UUID, keys []CitationKey) (map[uuid.UUID]CitationMeta, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.meta, nil
}

var errFakeLookup = errString("fake lookup error")

type errString string

func (e errString) Error() string { return string(e) }
