package context

// policy_test.go verifies the four built-in AuthorityPolicy implementations
// (YS-208 DoD "四 Policy Score 权重对齐 §5.1 表，单测验证排序；冲突关系保留
// 不合并"). It pins:
//   - ConflictsToSurface returns the §5.1 must-surface conflict types per policy.
//   - Score weights align the §5.1 table: under IntentSpec a document beats
//     code/memory/skill at equal signals; under IntentRevision code wins; etc.
//   - Score ordering is by blended score desc, stable on ties.
//   - NewAuthorityPolicy overlays a DB PolicyConfig (weights + conflicts)
//     without touching the scoring logic (§5.3).
//   - Conflict relations are retained on the candidate (not merged away) so
//     the Broker surfaces them side-by-side (§7.2).

import (
	"testing"

	"github.com/lynn901/mora/internal/domain"
)

// allFourTypes is one candidate per asset type at equal signals, so the only
// differentiator is the policy's per-type weight (§5.1). Equal Authority/
// Freshness/Confidence means the blended score ordering == the weight ordering.
func allFourTypes() []KnowledgeCandidate {
	conf := 0.5
	return []KnowledgeCandidate{
		{AssetID: id("doc"), AssetType: domain.AssetTypeDocument, Title: "doc", Authority: 0.5, Freshness: 0.5, Confidence: &conf},
		{AssetID: id("code"), AssetType: domain.AssetTypeCodebase, Title: "code", Authority: 0.5, Freshness: 0.5, Confidence: &conf},
		{AssetID: id("mem"), AssetType: domain.AssetTypeMemory, Title: "mem", Authority: 0.5, Freshness: 0.5, Confidence: &conf},
		{AssetID: id("skill"), AssetType: domain.AssetTypeSkill, Title: "skill", Authority: 0.5, Freshness: 0.5, Confidence: &conf},
	}
}

func TestPolicy_ConflictsToSurface_Spec(t *testing.T) {
	p := policyFor(IntentSpec)
	got := conflictSet(p.ConflictsToSurface())
	// §5.1: spec must surface old_spec + impl_drift.
	if !got["old_spec"] || !got["impl_drift"] {
		t.Errorf("spec ConflictsToSurface = %v, want {old_spec, impl_drift}", got)
	}
}

func TestPolicy_ConflictsToSurface_Revision(t *testing.T) {
	p := policyFor(IntentRevision)
	got := conflictSet(p.ConflictsToSurface())
	// §5.1: revision must surface contradicts + impl_drift.
	if !got["contradicts"] || !got["impl_drift"] {
		t.Errorf("revision ConflictsToSurface = %v, want {contradicts, impl_drift}", got)
	}
}

func TestPolicy_ConflictsToSurface_Rationale(t *testing.T) {
	p := policyFor(IntentRationale)
	got := conflictSet(p.ConflictsToSurface())
	// §5.1: rationale must surface contradicts + old_spec.
	if !got["contradicts"] || !got["old_spec"] {
		t.Errorf("rationale ConflictsToSurface = %v, want {contradicts, old_spec}", got)
	}
}

func TestPolicy_ConflictsToSurface_Procedure(t *testing.T) {
	p := policyFor(IntentProcedure)
	got := conflictSet(p.ConflictsToSurface())
	// §5.1: procedure must surface version_mismatch + impl_drift.
	if !got["version_mismatch"] || !got["impl_drift"] {
		t.Errorf("procedure ConflictsToSurface = %v, want {version_mismatch, impl_drift}", got)
	}
}

func TestPolicy_Score_SpecDocumentBeatsOthers(t *testing.T) {
	// §5.1 spec weights: doc 0.9 > code 0.5 > memory 0.4 > skill 0.3. At equal
	// signals, document ranks first.
	p := policyFor(IntentSpec)
	scored := p.Score(allFourTypes(), KnowledgeQuery{})
	if len(scored) != 4 {
		t.Fatalf("got %d scored, want 4", len(scored))
	}
	if scored[0].Candidate.AssetType != domain.AssetTypeDocument {
		t.Errorf("spec: top type = %q, want document", scored[0].Candidate.AssetType)
	}
	if scored[3].Candidate.AssetType != domain.AssetTypeSkill {
		t.Errorf("spec: bottom type = %q, want skill", scored[3].Candidate.AssetType)
	}
	assertDescending(t, scored)
}

func TestPolicy_Score_RevisionCodeBeatsOthers(t *testing.T) {
	// §5.1 revision weights: code 0.9 > doc 0.5 > skill 0.4 > memory 0.3.
	p := policyFor(IntentRevision)
	scored := p.Score(allFourTypes(), KnowledgeQuery{})
	if scored[0].Candidate.AssetType != domain.AssetTypeCodebase {
		t.Errorf("revision: top type = %q, want codebase", scored[0].Candidate.AssetType)
	}
	if scored[3].Candidate.AssetType != domain.AssetTypeMemory {
		t.Errorf("revision: bottom type = %q, want memory", scored[3].Candidate.AssetType)
	}
	assertDescending(t, scored)
}

func TestPolicy_Score_RationaleMemoryBeatsOthers(t *testing.T) {
	// §5.1 rationale weights: memory 0.9 > doc 0.8 > code 0.3 > skill 0.3.
	// memory ranks first; doc second.
	p := policyFor(IntentRationale)
	scored := p.Score(allFourTypes(), KnowledgeQuery{})
	if scored[0].Candidate.AssetType != domain.AssetTypeMemory {
		t.Errorf("rationale: top type = %q, want memory", scored[0].Candidate.AssetType)
	}
	if scored[1].Candidate.AssetType != domain.AssetTypeDocument {
		t.Errorf("rationale: second type = %q, want document", scored[1].Candidate.AssetType)
	}
	assertDescending(t, scored)
}

func TestPolicy_Score_ProcedureSkillBeatsOthers(t *testing.T) {
	// §5.1 procedure weights: skill 0.9 > doc 0.6 > code 0.4 = memory 0.4.
	p := policyFor(IntentProcedure)
	scored := p.Score(allFourTypes(), KnowledgeQuery{})
	if scored[0].Candidate.AssetType != domain.AssetTypeSkill {
		t.Errorf("procedure: top type = %q, want skill", scored[0].Candidate.AssetType)
	}
	assertDescending(t, scored)
}

func TestPolicy_Score_BlendRespectsAuthorityNotJustTypeWeight(t *testing.T) {
	// §5.2: Score blends authority/freshness/confidence × type weight. A
	// high-authority lower-weight type can beat a low-authority higher-weight
	// type. Under spec (doc weight 0.9, memory 0.4): a memory candidate with
	// near-1.0 authority beats a document with ~0 authority.
	p := policyFor(IntentSpec)
	conf := 0.5
	cands := []KnowledgeCandidate{
		{AssetType: domain.AssetTypeDocument, Authority: 0.0, Freshness: 0.5, Confidence: &conf}, // 0.9 * (0+0.15+0.1)=0.225
		{AssetType: domain.AssetTypeMemory, Authority: 1.0, Freshness: 1.0, Confidence: &conf},   // 0.4 * (0.5+0.3+0.1)=0.36
	}
	scored := p.Score(cands, KnowledgeQuery{})
	if scored[0].Candidate.AssetType != domain.AssetTypeMemory {
		t.Errorf("blend: top = %q, want memory (high authority beats low-weight doc)", scored[0].Candidate.AssetType)
	}
}

func TestPolicy_Score_NilConfidenceContributesZero(t *testing.T) {
	// §9.5: nil confidence → 0 in the blend. Two candidates equal except one
	// has nil confidence: the one with confidence scores higher.
	p := policyFor(IntentSpec)
	conf := 0.5
	cands := []KnowledgeCandidate{
		{AssetType: domain.AssetTypeDocument, Authority: 0.5, Freshness: 0.5, Confidence: nil},
		{AssetType: domain.AssetTypeDocument, Authority: 0.5, Freshness: 0.5, Confidence: &conf},
	}
	scored := p.Score(cands, KnowledgeQuery{})
	if scored[0].Candidate.Confidence == nil {
		t.Error("nil-confidence candidate ranked above the confidence one")
	}
}

func TestPolicy_Score_StableOnTies(t *testing.T) {
	// §5.2: ties preserve input order (sort.SliceStable). Two identical-score
	// candidates keep their input order.
	p := policyFor(IntentSpec)
	conf := 0.5
	cands := []KnowledgeCandidate{
		{AssetID: id("first"), AssetType: domain.AssetTypeDocument, Authority: 0.5, Freshness: 0.5, Confidence: &conf},
		{AssetID: id("second"), AssetType: domain.AssetTypeDocument, Authority: 0.5, Freshness: 0.5, Confidence: &conf},
	}
	scored := p.Score(cands, KnowledgeQuery{})
	if scored[0].Candidate.AssetID != id("first") {
		t.Errorf("stable tie: first should stay first; got %v", scored[0].Candidate.AssetID)
	}
}

func TestPolicy_Score_Empty(t *testing.T) {
	p := policyFor(IntentSpec)
	if got := p.Score(nil, KnowledgeQuery{}); got != nil {
		t.Errorf("Score(nil) = %v, want nil", got)
	}
}

func TestPolicy_NewAuthorityPolicy_OverlaysWeights(t *testing.T) {
	// §5.3: DB config overrides weights. Flip spec so memory (0.4→0.99) beats
	// document (0.9→0.01). The scoring logic is unchanged — only the weights.
	overlay := PolicyConfig{
		Weights: map[domain.AssetType]float64{
			domain.AssetTypeDocument: 0.01,
			domain.AssetTypeMemory:   0.99,
		},
	}
	p := NewAuthorityPolicy(IntentSpec, overlay)
	scored := p.Score(allFourTypes(), KnowledgeQuery{})
	if scored[0].Candidate.AssetType != domain.AssetTypeMemory {
		t.Errorf("overlay: top = %q, want memory (weight flipped)", scored[0].Candidate.AssetType)
	}
}

func TestPolicy_NewAuthorityPolicy_OverlaysConflicts(t *testing.T) {
	// §5.3: DB config overrides must-surface conflicts. Override spec's
	// conflicts with a custom set.
	overlay := PolicyConfig{
		MustSurfaceConflicts: []string{"custom_conflict"},
	}
	p := NewAuthorityPolicy(IntentSpec, overlay)
	got := conflictSet(p.ConflictsToSurface())
	if !got["custom_conflict"] {
		t.Errorf("overlay ConflictsToSurface = %v, want custom_conflict present", got)
	}
	// The default old_spec/impl_drift are replaced, not merged (overlay replaces).
	if got["old_spec"] {
		t.Errorf("overlay replaced defaults; old_spec should be gone: %v", got)
	}
}

func TestPolicy_NewAuthorityPolicy_ZeroConfigKeepsDefaults(t *testing.T) {
	// §5.3: a zero PolicyConfig (no DB row) leaves the §5.1 defaults intact.
	p := NewAuthorityPolicy(IntentSpec, PolicyConfig{})
	got := conflictSet(p.ConflictsToSurface())
	if !got["old_spec"] || !got["impl_drift"] {
		t.Errorf("zero config: spec conflicts = %v, want defaults {old_spec, impl_drift}", got)
	}
	scored := p.Score(allFourTypes(), KnowledgeQuery{})
	if scored[0].Candidate.AssetType != domain.AssetTypeDocument {
		t.Errorf("zero config: spec top = %q, want document (default)", scored[0].Candidate.AssetType)
	}
}

func TestPolicy_ConflictsRetainedOnCandidate(t *testing.T) {
	// §7.2 / DoD: conflict relations are retained on the candidate, not merged
	// away. Score must NOT drop a candidate's Relations. The candidate carrying
	// a contradicts relation survives scoring with the relation intact.
	p := policyFor(IntentRationale)
	otherID := id("old")
	cand := KnowledgeCandidate{
		AssetID: id("new"), AssetType: domain.AssetTypeDocument,
		Authority: 0.5, Freshness: 0.5,
		Relations: []RelationSummary{
			{RelationType: "contradicts", TargetID: otherID, TargetTitle: "旧方案"},
		},
	}
	scored := p.Score([]KnowledgeCandidate{cand}, KnowledgeQuery{})
	if len(scored) != 1 {
		t.Fatalf("got %d, want 1", len(scored))
	}
	if len(scored[0].Candidate.Relations) != 1 {
		t.Fatalf("contradicts relation dropped: %+v", scored[0].Candidate.Relations)
	}
	if scored[0].Candidate.Relations[0].RelationType != "contradicts" {
		t.Errorf("relation type not retained: %+v", scored[0].Candidate.Relations[0])
	}
	if scored[0].Candidate.Relations[0].TargetID != otherID {
		t.Errorf("relation target not retained: %+v", scored[0].Candidate.Relations[0])
	}
}

// assertDescending checks the scored slice is monotonically non-increasing.
func assertDescending(t *testing.T, scored []ScoredCandidate) {
	t.Helper()
	for i := 1; i < len(scored); i++ {
		if scored[i].Score > scored[i-1].Score {
			t.Errorf("not descending at %d: %v > %v", i, scored[i].Score, scored[i-1].Score)
		}
	}
}

func conflictSet(s []string) map[string]bool {
	out := make(map[string]bool, len(s))
	for _, c := range s {
		out[c] = true
	}
	return out
}
