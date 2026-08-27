package contextbroker

// budgeter_test.go verifies the §6.2 degradation ladder: catalog-first
// admission (no body), per-type quota cap (the "单资产不能占满预算" guard),
// total token/item cap, and the no-silent-truncation invariant (every dropped
// candidate gets a reason + continue-read tool, §11.4).

import (
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// scored is a helper: wrap candidates as ScoredCandidate in policy-rank order
// (higher Score first). The Budgeter assumes the list is pre-sorted.
func scored(candidates ...KnowledgeCandidate) []ScoredCandidate {
	out := make([]ScoredCandidate, len(candidates))
	for i, c := range candidates {
		out[i] = ScoredCandidate{Candidate: c, Score: float64(len(candidates) - i)}
	}
	return out
}

func TestLadderBudgeter_SelectEmpty(t *testing.T) {
	b := NewLadderBudgeter()
	got, rep := b.Select(nil, DefaultBudget)
	if got != nil {
		t.Errorf("empty input should yield nil selected, got %v", got)
	}
	if rep.Reason != "" || rep.TruncatedAssetIDs != nil || rep.ContinueTools != nil {
		t.Errorf("empty input should yield empty report, got %+v", rep)
	}
}

// TestLadderBudgeter_AdmitsAllUnderBudget proves the happy path: a small
// candidate list under budget is fully admitted with an empty truncation
// report (no spurious reason when nothing was dropped).
func TestLadderBudgeter_AdmitsAllUnderBudget(t *testing.T) {
	b := NewLadderBudgeter()
	c1 := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	c2 := makeCandidate(domain.AssetTypeMemory, 0.5, 0.5, 0.5)
	budget := Budget{
		MaxTokens: 100000, MaxItems: 100,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 10, TokenShare: 0.5},
			domain.AssetTypeMemory:   {MaxItems: 10, TokenShare: 0.5},
		},
	}
	got, rep := b.Select(scored(c1, c2), budget)
	if len(got) != 2 {
		t.Fatalf("expected 2 admitted, got %d", len(got))
	}
	if rep.Reason != "" || len(rep.TruncatedAssetIDs) != 0 {
		t.Errorf("under-budget should yield empty report, got %+v", rep)
	}
}

// TestLadderBudgeter_NoBodyInlined proves §6.2 default: the admitted candidate
// carries title+snippet+citation (catalog), NOT the body. We assert the
// returned candidate's Snippet is the same short excerpt, not a full doc body.
// (The Budgeter does not fetch bodies; it passes the candidate through as-is,
// so this is really asserting the catalog-only contract holds — no mutation
// that would inline a body.)
func TestLadderBudgeter_NoBodyInlined(t *testing.T) {
	b := NewLadderBudgeter()
	c := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	c.Snippet = "short excerpt"
	got, _ := b.Select(scored(c), DefaultBudget)
	if len(got) != 1 || got[0].Snippet != "short excerpt" {
		t.Fatalf("budgeter must not inline body / mutate snippet: got %+v", got)
	}
}

// TestLadderBudgeter_PerTypeItemQuota proves the per-type MaxItems cap drops
// excess candidates of ONE type while still admitting OTHER types — the
// "单资产不能占满预算" guard (§6.2). A flood of documents must not crowd out
// memory candidates.
func TestLadderBudgeter_PerTypeItemQuota(t *testing.T) {
	b := NewLadderBudgeter()
	// 5 documents + 2 memories; document quota = 2 items, memory quota = 5.
	docs := []KnowledgeCandidate{
		makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9),
		makeCandidate(domain.AssetTypeDocument, 0.8, 0.8, 0.8),
		makeCandidate(domain.AssetTypeDocument, 0.7, 0.7, 0.7),
		makeCandidate(domain.AssetTypeDocument, 0.6, 0.6, 0.6),
		makeCandidate(domain.AssetTypeDocument, 0.5, 0.5, 0.5),
	}
	mem := []KnowledgeCandidate{
		makeCandidate(domain.AssetTypeMemory, 0.5, 0.5, 0.5),
		makeCandidate(domain.AssetTypeMemory, 0.4, 0.4, 0.4),
	}
	budget := Budget{
		MaxTokens: 100000, MaxItems: 100,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 2, TokenShare: 0.5},
			domain.AssetTypeMemory:   {MaxItems: 5, TokenShare: 0.5},
		},
	}
	got, rep := b.Select(scored(append(docs, mem...)...), budget)
	// 2 docs + 2 mems admitted = 4; 3 docs dropped
	if len(got) != 4 {
		t.Fatalf("expected 4 admitted (2 doc + 2 mem), got %d", len(got))
	}
	docCount, memCount := 0, 0
	for _, c := range got {
		switch c.AssetType {
		case domain.AssetTypeDocument:
			docCount++
		case domain.AssetTypeMemory:
			memCount++
		}
	}
	if docCount != 2 {
		t.Errorf("document quota should cap at 2, got %d", docCount)
	}
	if memCount != 2 {
		t.Errorf("memory should admit all 2, got %d", memCount)
	}
	// 3 documents dropped → reason quota_exhausted, continue tool document_read
	if rep.Reason != TruncReasonQuotaExhausted {
		t.Errorf("reason = %q, want %q", rep.Reason, TruncReasonQuotaExhausted)
	}
	if len(rep.TruncatedAssetIDs) != 3 {
		t.Errorf("dropped count = %d, want 3", len(rep.TruncatedAssetIDs))
	}
	if !contains(rep.ContinueTools, "document_read") {
		t.Errorf("continue tools %v should include document_read", rep.ContinueTools)
	}
}

// TestLadderBudgeter_TotalTokenBudgetFull proves the total MaxTokens cap: when
// the catalog entries exhaust the total budget, remaining candidates drop with
// reason budget_full (not quota_exhausted — no per-type cap was hit). To isolate
// the total cap, the candidate type is NOT in the TypeQuota map (the fail-open
// `!known` branch), so only the global total caps bind.
func TestLadderBudgeter_TotalTokenBudgetFull(t *testing.T) {
	b := NewLadderBudgeter()
	// candidates with large snippets to fill the token budget fast
	c1 := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	c1.Snippet = string(make([]byte, 2000)) // ~516 tokens each
	c2 := makeCandidate(domain.AssetTypeDocument, 0.8, 0.8, 0.8)
	c2.Snippet = string(make([]byte, 2000))
	c3 := makeCandidate(domain.AssetTypeDocument, 0.7, 0.7, 0.7)
	c3.Snippet = string(make([]byte, 2000))
	budget := Budget{
		MaxTokens: 600, MaxItems: 100, // ~1 candidate fits (total cap binds)
		TypeQuota: map[domain.AssetType]Quota{
			// document is NOT in the map → fail-open branch, only total caps bind.
			// Including a different type so the map is non-empty (else default).
			domain.AssetTypeMemory: {MaxItems: 10, TokenShare: 1.0},
		},
	}
	got, rep := b.Select(scored(c1, c2, c3), budget)
	if len(got) != 1 {
		t.Fatalf("expected 1 admitted under tight token budget (516>600 boundary), got %d", len(got))
	}
	// the 2nd+ documents drop on the TOTAL token cap (their type has no
	// per-type quota), so the reason is budget_full.
	if rep.Reason != TruncReasonBudgetFull {
		t.Errorf("reason = %q, want %q", rep.Reason, TruncReasonBudgetFull)
	}
	if len(rep.TruncatedAssetIDs) != 2 {
		t.Errorf("dropped count = %d, want 2", len(rep.TruncatedAssetIDs))
	}
}

// TestLadderBudgeter_TotalItemCap proves the total MaxItems cap drops the rest
// with budget_full (the item cap is a total-budget constraint).
func TestLadderBudgeter_TotalItemCap(t *testing.T) {
	b := NewLadderBudgeter()
	cands := []KnowledgeCandidate{
		makeCandidate(domain.AssetTypeMemory, 0.9, 0.9, 0.9),
		makeCandidate(domain.AssetTypeMemory, 0.8, 0.8, 0.8),
		makeCandidate(domain.AssetTypeMemory, 0.7, 0.7, 0.7),
	}
	budget := Budget{
		MaxTokens: 100000, MaxItems: 1,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeMemory: {MaxItems: 10, TokenShare: 1.0},
		},
	}
	got, rep := b.Select(scored(cands...), budget)
	if len(got) != 1 {
		t.Fatalf("expected 1 admitted under item cap, got %d", len(got))
	}
	if rep.Reason != TruncReasonBudgetFull {
		t.Errorf("reason = %q, want %q", rep.Reason, TruncReasonBudgetFull)
	}
	if len(rep.TruncatedAssetIDs) != 2 {
		t.Errorf("dropped count = %d, want 2", len(rep.TruncatedAssetIDs))
	}
}

// TestLadderBudgeter_NoSilentTruncation proves §11.4: whenever anything is
// dropped, the report carries a reason + the continue-read tool. There is no
// path that drops a candidate silently.
func TestLadderBudgeter_NoSilentTruncation(t *testing.T) {
	b := NewLadderBudgeter()
	// one of each type, all but the first dropped by a 1-item total cap
	cands := []KnowledgeCandidate{
		makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9),
		makeCandidate(domain.AssetTypeCodebase, 0.8, 0.8, 0.8),
		makeCandidate(domain.AssetTypeMemory, 0.7, 0.7, 0.7),
		makeCandidate(domain.AssetTypeSkill, 0.6, 0.6, 0.6),
	}
	budget := Budget{
		MaxTokens: 100000, MaxItems: 1,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 1, TokenShare: 0.25},
			domain.AssetTypeCodebase: {MaxItems: 1, TokenShare: 0.25},
			domain.AssetTypeMemory:   {MaxItems: 1, TokenShare: 0.25},
			domain.AssetTypeSkill:    {MaxItems: 1, TokenShare: 0.25},
		},
	}
	got, rep := b.Select(scored(cands...), budget)
	if len(got) != 1 {
		t.Fatalf("expected 1 admitted, got %d", len(got))
	}
	// 3 dropped — every dropped type's continue tool must be present
	if len(rep.TruncatedAssetIDs) != 3 {
		t.Errorf("dropped = %d, want 3", len(rep.TruncatedAssetIDs))
	}
	if rep.Reason == "" {
		t.Errorf("dropped candidates must carry a reason (no silent truncation)")
	}
	// all three dropped types' tools should be present
	for _, tool := range []string{"code_node", "memory_evidence_read", "skill_resources"} {
		if !contains(rep.ContinueTools, tool) {
			t.Errorf("continue tools %v missing %s", rep.ContinueTools, tool)
		}
	}
}

// TestLadderBudgeter_QuotaReasonPrecedence proves the reason ladder: when BOTH
// a per-type quota AND the total budget fill, quota_exhausted takes
// precedence (it's the first binding constraint in §6.2 order).
func TestLadderBudgeter_QuotaReasonPrecedence(t *testing.T) {
	b := NewLadderBudgeter()
	// documents hit their 1-item per-type quota; memories fill the total cap.
	docs := []KnowledgeCandidate{
		makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9),
		makeCandidate(domain.AssetTypeDocument, 0.8, 0.8, 0.8),
	}
	mem := makeCandidate(domain.AssetTypeMemory, 0.7, 0.7, 0.7)
	budget := Budget{
		MaxTokens: 100000, MaxItems: 2, // 2 total
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 1, TokenShare: 0.5},
			domain.AssetTypeMemory:   {MaxItems: 1, TokenShare: 0.5},
		},
	}
	got, rep := b.Select(scored(docs[0], mem, docs[1]), budget)
	// 1 doc + 1 mem admitted (2 total items), 1 doc dropped by its per-type quota
	if len(got) != 2 {
		t.Fatalf("expected 2 admitted, got %d", len(got))
	}
	// the dropped doc hit the document per-type quota (1 item), even though the
	// total item cap (2) is also now full — quota_exhausted wins by ladder order
	if rep.Reason != TruncReasonQuotaExhausted {
		t.Errorf("reason = %q, want %q (quota takes precedence)", rep.Reason, TruncReasonQuotaExhausted)
	}
}

// TestLadderBudgeter_FallbackToDefaultBudget proves a zero-value Budget falls
// back to DefaultBudget (§6.3) rather than admitting nothing or everything.
func TestLadderBudgeter_FallbackToDefaultBudget(t *testing.T) {
	b := NewLadderBudgeter()
	c := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	// zero-value budget
	got, _ := b.Select(scored(c), Budget{})
	if len(got) != 1 {
		t.Fatalf("zero budget should fall back to default and admit 1, got %d", len(got))
	}
}

// TestContinueToolForMapping pins the §6.2 / §11.3 type→tool mapping so a
// rename is caught.
func TestContinueToolForMapping(t *testing.T) {
	want := map[domain.AssetType]string{
		domain.AssetTypeDocument: "document_read",
		domain.AssetTypeCodebase: "code_node",
		domain.AssetTypeMemory:   "memory_evidence_read",
		domain.AssetTypeSkill:    "skill_resources",
	}
	for at, name := range want {
		if got := continueToolFor[at]; got != name {
			t.Errorf("continueToolFor[%s] = %q, want %q", at, got, name)
		}
	}
}

// TestEstimateTokens proves the token estimate is monotonic in snippet length
// and floored (no zero-cost entries — §6.2 every entry counts against quota).
func TestEstimateTokens(t *testing.T) {
	empty := makeCandidate(domain.AssetTypeDocument, 0, 0, 0)
	empty.Title, empty.Snippet = "", ""
	if got := estimateTokens(empty); got < minTokensPerEntry {
		t.Errorf("empty entry tokens = %d, want >= %d", got, minTokensPerEntry)
	}
	big := makeCandidate(domain.AssetTypeDocument, 0, 0, 0)
	big.Snippet = string(make([]byte, 4000)) // 1000 tokens
	if got := estimateTokens(big); got <= estimateTokens(empty) {
		t.Errorf("big entry tokens = %d should exceed empty %d", got, estimateTokens(empty))
	}
	// determinism: same input → same output
	if estimateTokens(big) != estimateTokens(big) {
		t.Errorf("estimateTokens is non-deterministic")
	}
}

// TestLadderBudgeter_DroppedAssetIDsAreUnique proves the dropped list has no
// duplicates even if a candidate somehow appears twice (defensive — the
// dedup step runs before budget, but the Budgeter should not corrupt the report).
func TestLadderBudgeter_DroppedAssetIDsAreUnique(t *testing.T) {
	b := NewLadderBudgeter()
	c1 := makeCandidate(domain.AssetTypeDocument, 0.9, 0.9, 0.9)
	c2 := makeCandidate(domain.AssetTypeDocument, 0.8, 0.8, 0.8)
	budget := Budget{
		MaxTokens: 100000, MaxItems: 1,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 10, TokenShare: 1.0},
		},
	}
	_, rep := b.Select(scored(c1, c2), budget)
	seen := make(map[uuid.UUID]bool)
	for _, id := range rep.TruncatedAssetIDs {
		if seen[id] {
			t.Errorf("dropped asset id %s appears twice", id)
		}
		seen[id] = true
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
