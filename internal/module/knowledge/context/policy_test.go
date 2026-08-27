package contextbroker

// policy_test.go verifies the four built-in AuthorityPolicy implementations
// (§5.1): the §5.1 weight table per intent, that Score ranks candidates so the
// intent's primary-basis type wins, that ConflictsToSurface returns the right
// conflict types per intent, and that ApplyPolicyConfig overlays a DB-loaded
// PolicyConfig on the §5.1 defaults (§5.3 — config overrides weights +
// conflicts, not logic).

import (
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// makeCandidate is a tiny helper to build a KnowledgeCandidate with the signals
// the policy blend reads (Authority/Freshness/Confidence/Score/AssetType).
func makeCandidate(assetType domain.AssetType, authority, freshness, score float64) KnowledgeCandidate {
	c := KnowledgeCandidate{
		AssetID:    uuid.New(),
		AssetType:  assetType,
		Authority:  authority,
		Freshness:  freshness,
		Score:      score,
		Confidence: &score,
	}
	c.Citation = Citation{AssetID: c.AssetID, AssetType: assetType, Authority: authority, Confidence: &score}
	return c
}

func TestBuiltInPolicies_Intent(t *testing.T) {
	cases := []struct {
		policy AuthorityPolicy
		want   Intent
	}{
		{SpecPolicy(), IntentSpec},
		{RevisionPolicy(), IntentRevision},
		{RationalePolicy(), IntentRationale},
		{ProcedurePolicy(), IntentProcedure},
	}
	for _, c := range cases {
		if c.policy.Intent() != c.want {
			t.Errorf("%T: Intent() = %q, want %q", c.policy, c.policy.Intent(), c.want)
		}
	}
}

func TestBuiltInPolicies_ConflictsToSurface(t *testing.T) {
	// §5.1 must_surface_conflicts column per intent.
	cases := []struct {
		policy AuthorityPolicy
		want   []string
	}{
		{SpecPolicy(), []string{ConflictOldSpec, ConflictImplDrift}},
		{RevisionPolicy(), []string{ConflictDocMismatch}},
		{RationalePolicy(), []string{ConflictLowConfidence, ConflictSuperseded}},
		{ProcedurePolicy(), []string{ConflictVersionMismatch, ConflictMissingPerm}},
	}
	for _, c := range cases {
		got := c.policy.ConflictsToSurface()
		if !equalStrings(got, c.want) {
			t.Errorf("%T: ConflictsToSurface() = %v, want %v", c.policy, got, c.want)
		}
	}
}

// TestBuiltInPolicies_DefaultWeights pins the §5.1 weight table. This is the
// "Score 权重对齐 §5.1 表" DoD: if anyone changes the default weights the test
// fails, surfacing the deviation before it ships.
func TestBuiltInPolicies_DefaultWeights(t *testing.T) {
	// §5.1 table: document/code/memory/skill per intent.
	want := map[Intent]policyWeights{
		IntentSpec: {
			domain.AssetTypeDocument: 0.9, domain.AssetTypeCodebase: 0.5,
			domain.AssetTypeMemory: 0.4, domain.AssetTypeSkill: 0.3,
		},
		IntentRevision: {
			domain.AssetTypeDocument: 0.5, domain.AssetTypeCodebase: 0.9,
			domain.AssetTypeMemory: 0.3, domain.AssetTypeSkill: 0.4,
		},
		IntentRationale: {
			domain.AssetTypeDocument: 0.8, domain.AssetTypeCodebase: 0.3,
			domain.AssetTypeMemory: 0.9, domain.AssetTypeSkill: 0.3,
		},
		IntentProcedure: {
			domain.AssetTypeDocument: 0.6, domain.AssetTypeCodebase: 0.4,
			domain.AssetTypeMemory: 0.4, domain.AssetTypeSkill: 0.9,
		},
	}
	for intent, w := range want {
		got := defaultWeights[intent]
		if !equalWeights(got, w) {
			t.Errorf("defaultWeights[%s] = %v, want %v", intent, got, w)
		}
	}
}

// TestSpecPolicy_ScorePrimaryBasisWins proves §5.1: under IntentSpec, a
// document (weight 0.9) outranks code (0.5) even when the code candidate has a
// higher raw authority signal — because the per-type weight scales the blend so
// the intent's primary-basis type wins. This is the "系统不维护单一全局排序"
// guarantee: authority is intent-dependent.
func TestSpecPolicy_ScorePrimaryBasisWins(t *testing.T) {
	p := SpecPolicy()
	// code candidate has HIGHER raw authority (0.9) than the document (0.6),
	// but under spec intent document's weight 0.9 >> code's 0.5, so the
	// document must rank first.
	doc := makeCandidate(domain.AssetTypeDocument, 0.6, 0.6, 0.6)
	code := makeCandidate(domain.AssetTypeCodebase, 0.9, 0.9, 0.9)
	got := p.Score([]KnowledgeCandidate{code, doc}, KnowledgeQuery{})
	if len(got) != 2 {
		t.Fatalf("expected 2 scored, got %d", len(got))
	}
	// document should rank first despite lower raw authority
	if got[0].Candidate.AssetType != domain.AssetTypeDocument {
		t.Errorf("spec should rank document first: got %s first", got[0].Candidate.AssetType)
	}
	if got[1].Candidate.AssetType != domain.AssetTypeCodebase {
		t.Errorf("spec should rank code second: got %s second", got[1].Candidate.AssetType)
	}
	// sanity: the document's blended score must exceed the code's
	if got[0].Score <= got[1].Score {
		t.Errorf("document score %.4f should exceed code score %.4f", got[0].Score, got[1].Score)
	}
}

// TestRevisionPolicy_CodeOutranksDocument proves the mirror of the above: under
// IntentRevision, code (weight 0.9) outranks document (0.5) even when the
// document's raw authority is higher. Same signals, different intent, different
// ranking — the §5.1 intent-dependence guarantee.
func TestRevisionPolicy_CodeOutranksDocument(t *testing.T) {
	p := RevisionPolicy()
	doc := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	code := makeCandidate(domain.AssetTypeCodebase, 0.6, 0.6, 0.6)
	got := p.Score([]KnowledgeCandidate{doc, code}, KnowledgeQuery{})
	if got[0].Candidate.AssetType != domain.AssetTypeCodebase {
		t.Errorf("revision should rank code first: got %s", got[0].Candidate.AssetType)
	}
}

// TestPolicy_ScoreNilConfidenceFallsBackToProviderScore proves a candidate
// with no explicit Confidence signal is not zeroed — it falls back to the
// provider Score as the confidence proxy, so a missing signal does not sink an
// otherwise strong candidate.
func TestPolicy_ScoreNilConfidenceFallsBackToProviderScore(t *testing.T) {
	p := SpecPolicy()
	noConf := makeCandidate(domain.AssetTypeDocument, 0.8, 0.8, 0.8)
	noConf.Confidence = nil // engine emitted no confidence
	withConf := makeCandidate(domain.AssetTypeDocument, 0.8, 0.8, 0.8)
	got := p.Score([]KnowledgeCandidate{noConf, withConf}, KnowledgeQuery{})
	// same signals, same type → same blended score regardless of nil confidence
	if got[0].Score != got[1].Score {
		t.Errorf("nil-confidence candidate scored differently: %.4f vs %.4f", got[0].Score, got[1].Score)
	}
}

// TestPolicy_ScoreDoesNotDropCandidates proves Score only RANKS, never drops —
// even a zero-authority, zero-freshness candidate stays in the output for
// DedupAndKeepConflicts / Budgeter to handle. Dropping is the Budgeter's job
// (§6.2), not the policy's.
func TestPolicy_ScoreDoesNotDropCandidates(t *testing.T) {
	p := SpecPolicy()
	strong := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	weak := makeCandidate(domain.AssetTypeMemory, 0, 0, 0) // would be excluded by §7.2 exclude_when, but Score keeps it
	got := p.Score([]KnowledgeCandidate{strong, weak}, KnowledgeQuery{})
	if len(got) != 2 {
		t.Errorf("Score must not drop candidates: got %d, want 2", len(got))
	}
}

// TestPolicy_ScoreStableOrdering proves the tie-break is deterministic: two
// candidates with identical signals rank by AssetID ascending, so the same
// input always produces the same order across runs (§9.5 stable ranking).
func TestPolicy_ScoreStableOrdering(t *testing.T) {
	p := SpecPolicy()
	a := makeCandidate(domain.AssetTypeDocument, 0.5, 0.5, 0.5)
	b := makeCandidate(domain.AssetTypeDocument, 0.5, 0.5, 0.5)
	// ensure a's AssetID < b's so we can assert a ranks first
	if a.AssetID.String() > b.AssetID.String() {
		a, b = b, a
	}
	got := p.Score([]KnowledgeCandidate{b, a}, KnowledgeQuery{})
	if got[0].Candidate.AssetID != a.AssetID {
		t.Errorf("tie-break should rank lower AssetID first: got %s", got[0].Candidate.AssetID)
	}
}

// TestApplyPolicyConfig_OverridesWeights proves §5.3: a DB-loaded PolicyConfig
// replaces the §5.1 default weights for the intent, so a PM-tuned row changes
// ranking without a code change.
func TestApplyPolicyConfig_OverridesWeights(t *testing.T) {
	builtIn := SpecPolicy()
	// override: bump code weight above document so under the override, code wins
	cfg := PolicyConfig{
		Weights: policyWeights{
			domain.AssetTypeDocument: 0.1,
			domain.AssetTypeCodebase: 0.9,
			domain.AssetTypeMemory:   0.1,
			domain.AssetTypeSkill:    0.1,
		},
		MustSurfaceConflicts: []string{ConflictImplDrift},
	}
	override := ApplyPolicyConfig(builtIn, cfg)

	doc := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	code := makeCandidate(domain.AssetTypeCodebase, 0.5, 0.5, 0.5)

	// under the BUILT-IN spec weights, doc (0.9*0.9=0.81 blend-scaling) beats
	// code (0.5 blend * 0.9 authority...). Confirm the override FLIPS it.
	gotOverride := override.Score([]KnowledgeCandidate{doc, code}, KnowledgeQuery{})
	if gotOverride[0].Candidate.AssetType != domain.AssetTypeCodebase {
		t.Errorf("override should make code win under spec intent: got %s first", gotOverride[0].Candidate.AssetType)
	}
	// and the original built-in must be UNCHANGED (no shared mutable state)
	gotOriginal := builtIn.Score([]KnowledgeCandidate{doc, code}, KnowledgeQuery{})
	if gotOriginal[0].Candidate.AssetType != domain.AssetTypeDocument {
		t.Errorf("built-in policy should be unmutated by ApplyPolicyConfig: got %s first", gotOriginal[0].Candidate.AssetType)
	}
}

// TestApplyPolicyConfig_OverridesConflicts proves §5.3: the DB config's
// MustSurfaceConflicts replaces the built-in conflict list.
func TestApplyPolicyConfig_OverridesConflicts(t *testing.T) {
	builtIn := SpecPolicy()
	cfg := PolicyConfig{MustSurfaceConflicts: []string{"custom_conflict"}}
	override := ApplyPolicyConfig(builtIn, cfg)
	got := override.ConflictsToSurface()
	if !equalStrings(got, []string{"custom_conflict"}) {
		t.Errorf("override conflicts = %v, want [custom_conflict]", got)
	}
	// original unchanged
	if !equalStrings(builtIn.ConflictsToSurface(), []string{ConflictOldSpec, ConflictImplDrift}) {
		t.Errorf("built-in conflicts mutated by ApplyPolicyConfig: %v", builtIn.ConflictsToSurface())
	}
}

// TestApplyPolicyConfig_EmptyConfigIsNoOp proves a nil/empty config returns the
// built-in unchanged (§5.3 "missing row → built-in defaults").
func TestApplyPolicyConfig_EmptyConfigIsNoOp(t *testing.T) {
	builtIn := SpecPolicy()
	override := ApplyPolicyConfig(builtIn, PolicyConfig{})
	// weights should still be the §5.1 defaults
	doc := makeCandidate(domain.AssetTypeDocument, 0.6, 0.6, 0.6)
	code := makeCandidate(domain.AssetTypeCodebase, 0.9, 0.9, 0.9)
	got := override.Score([]KnowledgeCandidate{code, doc}, KnowledgeQuery{})
	if got[0].Candidate.AssetType != domain.AssetTypeDocument {
		t.Errorf("empty config should not change spec ranking: got %s first", got[0].Candidate.AssetType)
	}
}

func TestPolicyForIntent(t *testing.T) {
	cases := []struct {
		intent Intent
		want   Intent
	}{
		{IntentSpec, IntentSpec},
		{IntentRevision, IntentRevision},
		{IntentRationale, IntentRationale},
		{IntentProcedure, IntentProcedure},
		{"unknown", ""}, // nil policy
	}
	for _, c := range cases {
		p := PolicyForIntent(c.intent)
		if c.intent == "unknown" {
			if p != nil {
				t.Errorf("unknown intent should yield nil policy, got %T", p)
			}
			continue
		}
		if p == nil || p.Intent() != c.want {
			t.Errorf("PolicyForIntent(%s): got %v, want intent %s", c.intent, p, c.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalWeights(a, b policyWeights) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
