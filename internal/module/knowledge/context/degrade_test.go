package context

// degrade_test.go pins the §7.3 three-state distinguishability (D8). The four
// states must be mutually exclusive and exhaustive over the empty-result
// shapes; collapsing any two is a leak (§11.4 "不得把失败解释为「没有知识」").
//
// The ContextResponse three-state methods are pure functions over (Candidates,
// DegradedSources, preCheckCandidateCount). These tests exercise each method
// directly + assert the mutual-exclusivity invariants the handler relies on.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// ---------------------------------------------------------------------------
// HasResults: at least one candidate survived
// ---------------------------------------------------------------------------

func TestThreeState_HasResults(t *testing.T) {
	r := ContextResponse{
		Candidates: []KnowledgeCandidate{{AssetID: uuid.New(), AssetType: domain.AssetTypeDocument}},
	}
	if !r.HasResults() {
		t.Error("HasResults=false, want true")
	}
	if r.IsEmptyAuthorized() {
		t.Error("IsEmptyAuthorized=true, want false (has results)")
	}
	if r.IsEmptyNoResults() {
		t.Error("IsEmptyNoResults=true, want false (has results)")
	}
	if r.IsProviderDown() {
		t.Error("IsProviderDown=true, want false (has results)")
	}
}

// ---------------------------------------------------------------------------
// IsEmptyAuthorized: candidates existed, all denied by post-check (no provider
// failure). The authorized-empty state (§11.5 — surfaced as empty, never 403).
// ---------------------------------------------------------------------------

func TestThreeState_IsEmptyAuthorized(t *testing.T) {
	r := ContextResponse{
		Candidates:             nil,
		DegradedSources:        nil,
		preCheckCandidateCount: 3, // candidates existed
	}
	if !r.IsEmptyAuthorized() {
		t.Error("IsEmptyAuthorized=false, want true (candidates existed, all denied)")
	}
	if r.HasResults() {
		t.Error("HasResults=true, want false")
	}
	if r.IsEmptyNoResults() {
		t.Error("IsEmptyNoResults=true, want false (candidates existed)")
	}
	if r.IsProviderDown() {
		t.Error("IsProviderDown=true, want false (no provider failure)")
	}
}

// ---------------------------------------------------------------------------
// IsEmptyNoResults: every port returned empty, no port failed — the genuine
// "nothing exists" state (§11.5 — the only state where that claim is accurate).
// ---------------------------------------------------------------------------

func TestThreeState_IsEmptyNoResults(t *testing.T) {
	r := ContextResponse{
		Candidates:             nil,
		DegradedSources:        nil,
		preCheckCandidateCount: 0, // no candidates were ever fetched
	}
	if !r.IsEmptyNoResults() {
		t.Error("IsEmptyNoResults=false, want true (genuine empty)")
	}
	if r.HasResults() {
		t.Error("HasResults=true, want false")
	}
	if r.IsEmptyAuthorized() {
		t.Error("IsEmptyAuthorized=true, want false (no candidates existed)")
	}
	if r.IsProviderDown() {
		t.Error("IsProviderDown=true, want false (no provider failure)")
	}
}

// ---------------------------------------------------------------------------
// IsProviderDown: at least one port failed AND no candidate survived. The
// caller must NOT conclude the asset does not exist (§11.4 / §7.3).
// ---------------------------------------------------------------------------

func TestThreeState_IsProviderDown(t *testing.T) {
	r := ContextResponse{
		Candidates: nil,
		DegradedSources: []DegradedSource{
			{AssetType: domain.AssetTypeCodebase, Reason: DegradedProviderTimeout},
		},
	}
	if !r.IsProviderDown() {
		t.Error("IsProviderDown=false, want true (port failed, no results)")
	}
	if r.HasResults() {
		t.Error("HasResults=true, want false")
	}
	if r.IsEmptyAuthorized() {
		t.Error("IsEmptyAuthorized=true, want false (provider is down, not authorized-empty)")
	}
	if r.IsEmptyNoResults() {
		t.Error("IsEmptyNoResults=true, want false (provider is down, not genuinely empty)")
	}
}

// ---------------------------------------------------------------------------
// §7.3 invariant: when candidates survived AND a port degraded, HasResults
// wins — partial success is "has results", not "provider down". The degraded
// list is carried alongside so the caller sees the partial state.
// ---------------------------------------------------------------------------

func TestThreeState_PartialSuccessHasResultsNotProviderDown(t *testing.T) {
	r := ContextResponse{
		Candidates: []KnowledgeCandidate{{AssetID: uuid.New(), AssetType: domain.AssetTypeDocument}},
		DegradedSources: []DegradedSource{
			{AssetType: domain.AssetTypeCodebase, Reason: DegradedProviderTimeout},
		},
		preCheckCandidateCount: 2,
	}
	if !r.HasResults() {
		t.Error("HasResults=false, want true (partial success)")
	}
	if r.IsProviderDown() {
		t.Error("IsProviderDown=true, want false (candidates survived → not provider down)")
	}
}

// ---------------------------------------------------------------------------
// §7.3 invariant: provider down takes precedence over authorized-empty when no
// candidate survived. If ports failed AND pre-check had candidates, the empty
// result is still a provider fault first (the caller cannot know whether the
// denied candidates would have survived absent the failure). §11.4.
// ---------------------------------------------------------------------------

func TestThreeState_ProviderDownBeatsAuthorizedEmpty(t *testing.T) {
	r := ContextResponse{
		Candidates:             nil,
		DegradedSources:        []DegradedSource{{AssetType: domain.AssetTypeDocument, Reason: DegradedQdrantUnavailable}},
		preCheckCandidateCount: 5,
	}
	if !r.IsProviderDown() {
		t.Error("IsProviderDown=false, want true (provider fault dominates when empty)")
	}
	if r.IsEmptyAuthorized() {
		t.Error("IsEmptyAuthorized=true, want false (IsProviderDown dominates, §11.4)")
	}
}

// ---------------------------------------------------------------------------
// DegradedReason constants are the §7.3 vocabulary — stable metric labels.
// ---------------------------------------------------------------------------

func TestDegradedReason_Vocabulary(t *testing.T) {
	want := map[DegradedReason]bool{
		DegradedProviderTimeout:    true,
		DegradedProviderError:      true,
		DegradedQdrantUnavailable:  true,
		DegradedFTSFallback:        true,
		DegradedRerankUnavailable:  true,
	}
	got := map[DegradedReason]bool{
		"provider_timeout":   true,
		"provider_error":     true,
		"qdrant_unavailable": true,
		"fts_fallback":       true,
		"rerank_unavailable": true,
	}
	for k := range want {
		if !got[k] {
			t.Errorf("reason %q is not the §7.3 vocabulary value", k)
		}
	}
}
