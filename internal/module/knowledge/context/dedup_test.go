package context

// dedup_test.go pins the §7.2 dedup + conflict retention + exclude behavior
// (D7). It exercises DedupAndKeepConflicts as a pure function over candidate
// slices + the built-in policies, so the §7.2 contract is verified without the
// broker fan-out (broker_test.go covers the integrated path).

import (
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// dcand builds a dedup-test candidate with sane defaults. signals are
// (authority, freshness).
func dcand(assetID uuid.UUID, atype domain.AssetType, title string, signals ...float64) KnowledgeCandidate {
	var auth, fresh float64
	if len(signals) > 0 {
		auth = signals[0]
	}
	if len(signals) > 1 {
		fresh = signals[1]
	}
	return KnowledgeCandidate{
		AssetID:   assetID,
		AssetType: atype,
		Title:     title,
		Snippet:   title,
		Score:     auth,
		Authority: auth,
		Freshness: fresh,
	}
}

// ---------------------------------------------------------------------------
// asset_id dedup: same asset, multiple versions → keep newest ready
// ---------------------------------------------------------------------------

func TestDedup_SameAssetKeepsNewestReady(t *testing.T) {
	a := uuid.New()
	c1 := dcand(a, domain.AssetTypeDocument, "v1", 0.5, 0.3)
	c2 := dcand(a, domain.AssetTypeDocument, "v2", 0.5, 0.9) // newer
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentSpec))
	if len(out) != 1 {
		t.Fatalf("dedup = %d, want 1 (same asset collapsed)", len(out))
	}
	if out[0].Title != "v2" {
		t.Errorf("kept = %q, want v2 (newest ready)", out[0].Title)
	}
}

// ---------------------------------------------------------------------------
// content_hash dedup: same content, different assets → keep one
// ---------------------------------------------------------------------------

func TestDedup_SameContentDifferentAssetsKeepsOne(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	c1 := dcand(a, domain.AssetTypeDocument, "doc A", 0.9, 0.9)
	c1.ContentHash = "hash-x"
	c2 := dcand(b, domain.AssetTypeDocument, "doc B dup", 0.5, 0.5)
	c2.ContentHash = "hash-x"
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentSpec))
	if len(out) != 1 {
		t.Fatalf("dedup = %d, want 1 (same content collapsed)", len(out))
	}
	if out[0].Title != "doc A" {
		t.Errorf("kept = %q, want doc A (higher freshness/score)", out[0].Title)
	}
}

// ---------------------------------------------------------------------------
// §7.2 conflict pair on same asset: both pinned members kept side-by-side
// ---------------------------------------------------------------------------

func TestDedup_SameAssetConflictPairKeptSideBySide(t *testing.T) {
	a := uuid.New()
	target := uuid.New()
	c1 := dcand(a, domain.AssetTypeMemory, "v2 new", 0.6, 0.9)
	c1.Relations = []RelationSummary{{RelationType: "contradicts", TargetID: target, TargetTitle: "v1"}}
	c2 := dcand(a, domain.AssetTypeMemory, "v1 old", 0.4, 0.3)
	c2.ConflictTags = []string{"impl_drift"} // revision surfaces impl_drift too
	// IntentRevision surfaces {contradicts, impl_drift}: BOTH members carry a
	// surfaceable conflict → both pinned → kept side-by-side.
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentRevision))
	if len(out) != 2 {
		t.Fatalf("dedup = %d, want 2 (same-asset conflict pair kept side-by-side, §7.2): %+v", len(out), out)
	}
}

// ---------------------------------------------------------------------------
// §7.2 conflict pair on different assets: both kept (no same-asset collapse)
// ---------------------------------------------------------------------------

func TestDedup_DifferentAssetConflictPairKept(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	c1 := dcand(a, domain.AssetTypeMemory, "decision A", 0.8, 0.8)
	c1.Relations = []RelationSummary{{RelationType: "contradicts", TargetID: b, TargetTitle: "decision B"}}
	c2 := dcand(b, domain.AssetTypeMemory, "decision B", 0.7, 0.7)
	// rationale surfaces {contradicts, old_spec}. c1 is pinned (contradicts);
	// c2 is not pinned but it is a DIFFERENT asset → dedup keeps it.
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentRationale))
	if len(out) != 2 {
		t.Errorf("dedup = %d, want 2 (different-asset conflict pair): %+v", len(out), out)
	}
}

// ---------------------------------------------------------------------------
// §7.2 exclude_when: deprecated / version_mismatch dropped
// ---------------------------------------------------------------------------

func TestDedup_ExcludeWhenDropsDeprecated(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	c1 := dcand(a, domain.AssetTypeDocument, "live doc", 0.9, 0.9)
	c2 := dcand(b, domain.AssetTypeDocument, "old doc", 0.5, 0.5)
	c2.ConflictTags = []string{"deprecated"} // spec excludes deprecated
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentSpec))
	if len(out) != 1 {
		t.Fatalf("dedup = %d, want 1 (deprecated excluded): %+v", len(out), out)
	}
	if out[0].Title != "live doc" {
		t.Errorf("kept = %q, want live doc", out[0].Title)
	}
}

func TestDedup_ExcludeWhenDropsVersionMismatch(t *testing.T) {
	a := uuid.New()
	c := dcand(a, domain.AssetTypeCodebase, "stale code", 0.5, 0.5)
	c.ConflictTags = []string{"version_mismatch"} // revision excludes version_mismatch
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c}, policyFor(IntentRevision))
	if len(out) != 0 {
		t.Errorf("dedup = %d, want 0 (version_mismatch excluded): %+v", len(out), out)
	}
}

// ---------------------------------------------------------------------------
// §7.2 mutual-exclusion defense: a tag in BOTH exclude_when and
// must_surface_conflicts → must_surface wins (NOT excluded)
// ---------------------------------------------------------------------------

func TestDedup_MustSurfaceWinsOverExcludeWhen(t *testing.T) {
	// Build a custom policy where "old_spec" is in BOTH exclude_when AND
	// must_surface_conflicts. The PUT handler rejects this config (§7.2), but
	// DedupAndKeepConflicts defends in depth: must_surface wins so the conflict
	// is never dropped before dedup.
	a := uuid.New()
	c := dcand(a, domain.AssetTypeDocument, "old spec", 0.4, 0.3)
	c.ConflictTags = []string{"old_spec"}
	colliding := NewAuthorityPolicy(IntentSpec, PolicyConfig{
		MustSurfaceConflicts: []string{"old_spec"},
		ExcludeWhen:          []string{"old_spec"},
		Weights:              map[domain.AssetType]float64{domain.AssetTypeDocument: 0.9},
	})
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c}, colliding)
	if len(out) != 1 {
		t.Errorf("dedup = %d, want 1 (must_surface wins over exclude_when, §7.2 mutual-exclusion): %+v", len(out), out)
	}
}

// ---------------------------------------------------------------------------
// §7.2: a pinned conflict carrier survives content_hash dedup even when a
// higher-freshness duplicate exists on a different asset
// ---------------------------------------------------------------------------

func TestDedup_PinnedSurvivesContentHashDedup(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	target := uuid.New()
	c1 := dcand(a, domain.AssetTypeMemory, "contradicting", 0.3, 0.3) // low freshness
	c1.ContentHash = "hash-y"
	c1.Relations = []RelationSummary{{RelationType: "contradicts", TargetID: target, TargetTitle: "target"}}
	c2 := dcand(b, domain.AssetTypeMemory, "fresh dup", 0.9, 0.9) // would win newest-ready
	c2.ContentHash = "hash-y"
	// revision surfaces contradicts → c1 pinned → survives content_hash dedup
	// even though c2 is the newest-ready.
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentRevision))
	foundC1 := false
	for _, c := range out {
		if c.AssetID == a {
			foundC1 = true
		}
	}
	if !foundC1 {
		t.Errorf("pinned c1 (contradicts) was dropped by content_hash dedup, want kept (§7.2): %+v", out)
	}
}

// ---------------------------------------------------------------------------
// §7.2: empty ContentHash candidates are never deduped by content_hash
// ---------------------------------------------------------------------------

func TestDedup_EmptyContentHashNotDeduped(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	c1 := dcand(a, domain.AssetTypeDocument, "doc A", 0.5, 0.5) // no hash
	c2 := dcand(b, domain.AssetTypeDocument, "doc B", 0.5, 0.5) // no hash
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentSpec))
	if len(out) != 2 {
		t.Errorf("dedup = %d, want 2 (empty hashes not collapsed): %+v", len(out), out)
	}
}

// ---------------------------------------------------------------------------
// §7.2: nil policy → no exclusion, no pinning, pure asset_id/content_hash dedup
// ---------------------------------------------------------------------------

func TestDedup_NilPolicyPureDedup(t *testing.T) {
	a := uuid.New()
	c1 := dcand(a, domain.AssetTypeDocument, "v1", 0.5, 0.3)
	c2 := dcand(a, domain.AssetTypeDocument, "v2", 0.5, 0.9)
	// A candidate that WOULD be excluded under a real policy (deprecated tag) —
	// with nil policy, no exclusion applies, so only the same-asset collapse runs.
	c2.ConflictTags = []string{"deprecated"}
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, nil)
	if len(out) != 1 {
		t.Fatalf("dedup = %d, want 1 (nil policy: no exclude, same-asset collapse only): %+v", len(out), out)
	}
	if out[0].Title != "v2" {
		t.Errorf("kept = %q, want v2 (newest ready, nil policy)", out[0].Title)
	}
}

// ---------------------------------------------------------------------------
// §7.2: Relations (directed edges with TargetID) surface conflicts the same as
// ConflictTags (candidate-self labels) — the union is the pinning signal
// ---------------------------------------------------------------------------

func TestDedup_RelationsSurfaceConflictsLikeTags(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	target := uuid.New()
	// Two candidates on DIFFERENT assets. c1 surfaces via Relations
	// (contradicts); c2 surfaces via ConflictTags (impl_drift). Under
	// IntentRevision both are pinned → both kept.
	c1 := dcand(a, domain.AssetTypeMemory, "via relation", 0.5, 0.5)
	c1.Relations = []RelationSummary{{RelationType: "contradicts", TargetID: target, TargetTitle: "target"}}
	c2 := dcand(b, domain.AssetTypeMemory, "via tag", 0.5, 0.5)
	c2.ConflictTags = []string{"impl_drift"}
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2}, policyFor(IntentRevision))
	if len(out) != 2 {
		t.Errorf("dedup = %d, want 2 (Relations + ConflictTags union both pinned): %+v", len(out), out)
	}
}

// ---------------------------------------------------------------------------
// §7.2 priority: asset_id dedup runs before content_hash dedup
// ---------------------------------------------------------------------------

func TestDedup_AssetIDBeforeContentHash(t *testing.T) {
	// Three candidates: two on asset A (same content), one on asset B (same
	// content). asset_id dedup first collapses A's pair to the newest ready;
	// content_hash then collapses the A-survivor + B to one.
	a, b := uuid.New(), uuid.New()
	c1 := dcand(a, domain.AssetTypeDocument, "A v1", 0.5, 0.3)
	c1.ContentHash = "hash-z"
	c2 := dcand(a, domain.AssetTypeDocument, "A v2", 0.5, 0.9) // newest on A
	c2.ContentHash = "hash-z"
	c3 := dcand(b, domain.AssetTypeDocument, "B", 0.5, 0.5)
	c3.ContentHash = "hash-z"
	out := DedupAndKeepConflicts([]KnowledgeCandidate{c1, c2, c3}, policyFor(IntentSpec))
	if len(out) != 1 {
		t.Errorf("dedup = %d, want 1 (asset_id then content_hash): %+v", len(out), out)
	}
	if out[0].Title != "A v2" {
		t.Errorf("kept = %q, want A v2 (asset_id newest ready wins content_hash tiebreak)", out[0].Title)
	}
}

// ---------------------------------------------------------------------------
// empty input → nil (not an empty slice, so callers see "nothing to dedup")
// ---------------------------------------------------------------------------

func TestDedup_EmptyInputNil(t *testing.T) {
	out := DedupAndKeepConflicts(nil, policyFor(IntentSpec))
	if out != nil {
		t.Errorf("dedup(nil) = %v, want nil", out)
	}
}
