package context

// budgeter_test.go verifies Budgeter.Select (YS-208 DoD "Budgeter.Select 降级
// 阶梯，TruncationReport 含原因+工具名，单测验证单资产不能占满预算"). It pins:
//   - Degradation ladder: directory + summary + citation only; the body is not
//     inlined (§6.2). The selected candidate carries its snippet, not a body.
//   - Per-type quota (MaxItems) stops a type early.
//   - Single-asset cap: one long candidate cannot fill the budget (§6.2
//     TokenShare / MaxItems → maxPerAsset).
//   - TruncationReport carries the reason + truncated asset_ids + the
//     continue-read tool names (§11.4 — no silent truncation).
//   - Empty/zero budgets degrade gracefully (no panic, no silent drop).

import (
	"strings"
	"testing"

	"github.com/lynn901/mora/internal/domain"
)

func defaultBudget() Budget {
	return Budget{
		MaxTokens: 1000,
		MaxItems:  10,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 5, TokenShare: 0.5},
			domain.AssetTypeCodebase: {MaxItems: 3, TokenShare: 0.3},
			domain.AssetTypeMemory:   {MaxItems: 2, TokenShare: 0.2},
		},
		Timeout: 0,
	}
}

func TestBudgeter_DegradationLadder_SnippetOnlyNoBody(t *testing.T) {
	// §6.2: the selected candidate is at directory + summary + citation. The
	// body is NOT inlined — the selected candidate carries its Snippet (the
	// redacted excerpt), and the body is a progressive read via the continue
	// tool. Here everything fits: no truncation.
	b := NewBudgeter()
	longBody := strings.Repeat("a", 200) // a snippet, not a body
	cand := KnowledgeCandidate{
		AssetID: id("doc1"), AssetType: domain.AssetTypeDocument,
		Title: "doc1", Snippet: longBody,
	}
	selected, trunc := b.Select([]ScoredCandidate{{Candidate: cand, Score: 0.9}}, defaultBudget())
	if len(selected) != 1 {
		t.Fatalf("got %d selected, want 1", len(selected))
	}
	if selected[0].Snippet != longBody {
		t.Error("degradation ladder dropped the snippet — it should carry the summary")
	}
	if trunc.Reason != "" {
		t.Errorf("no truncation expected, got %v", trunc)
	}
}

func TestBudgeter_PerTypeMaxItemsStopsType(t *testing.T) {
	// §6.1 MaxItems per type: 5 documents allowed. 6 documents → 1 truncated,
	// reason quota_exhausted, continue tool document_read.
	b := NewBudgeter()
	cands := make([]ScoredCandidate, 0, 6)
	for i := 0; i < 6; i++ {
		cands = append(cands, ScoredCandidate{
			Candidate: KnowledgeCandidate{
				AssetID: id(docName(i)), AssetType: domain.AssetTypeDocument,
				Title: docName(i), Snippet: "x",
			},
			Score: 0.9 - float64(i)*0.01, // distinct scores → stable order
		})
	}
	selected, trunc := b.Select(cands, defaultBudget())
	if len(selected) != 5 {
		t.Errorf("got %d selected, want 5 (doc MaxItems)", len(selected))
	}
	if trunc.Reason != TruncReasonQuotaExhausted {
		t.Errorf("Reason = %q, want quota_exhausted", trunc.Reason)
	}
	if len(trunc.TruncatedAssetIDs) != 1 {
		t.Errorf("truncated ids = %v, want 1", trunc.TruncatedAssetIDs)
	}
	if !contains(trunc.ContinueTools, "document_read") {
		t.Errorf("ContinueTools = %v, want document_read", trunc.ContinueTools)
	}
}

func TestBudgeter_SingleAssetCannotFillBudget(t *testing.T) {
	// §6.2 / DoD: a single asset cannot fill the budget. One very long document
	// snippet is capped at maxPerAsset = MaxTokens*TokenShare/MaxItems. With
	// MaxTokens=1000, doc TokenShare=0.5, MaxItems=5 → maxPerAsset=100 tokens =
	// ~400 chars. A 10000-char snippet is capped, leaving budget for other
	// types — it does NOT squeeze them out.
	b := NewBudgeter()
	hugeDoc := KnowledgeCandidate{
		AssetID: id("huge"), AssetType: domain.AssetTypeDocument,
		Title: "huge", Snippet: strings.Repeat("b", 10000), // ~2500 tokens uncapped
	}
	smallMem := KnowledgeCandidate{
		AssetID: id("small"), AssetType: domain.AssetTypeMemory,
		Title: "small", Snippet: "tiny", // ~1 token
	}
	scored := []ScoredCandidate{
		{Candidate: hugeDoc, Score: 0.9},
		{Candidate: smallMem, Score: 0.5},
	}
	selected, _ := b.Select(scored, defaultBudget())
	// Both survive: the huge doc is capped at ~100 tokens (≤400 chars), the
	// small memory fits in its own quota. If the single-asset cap failed, the
	// huge doc would consume the whole 1000-token budget and the memory would
	// be truncated.
	foundDoc, foundMem := false, false
	for _, c := range selected {
		if c.AssetID == id("huge") {
			foundDoc = true
		}
		if c.AssetID == id("small") {
			foundMem = true
		}
	}
	if !foundDoc {
		t.Error("huge doc dropped — it should be selected (capped, not dropped)")
	}
	if !foundMem {
		t.Error("small memory dropped — single-asset cap should leave room; it did not")
	}
}

func TestBudgeter_BudgetFullStopsGlobally(t *testing.T) {
	// §6.1 MaxTokens: a tight total budget truncates candidates with reason
	// budget_full. Two types each with generous per-type caps (TokenShare 0.6 +
	// 0.6 > 1.0, so neither per-type cap fires first); the GLOBAL cap truncates.
	// Each candidate capped to maxPerAsset = MaxTokens*TokenShare/MaxItems.
	b := NewBudgeter()
	budget := Budget{
		MaxTokens: 10,
		MaxItems:  10,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 10, TokenShare: 0.6},
			domain.AssetTypeMemory:   {MaxItems: 10, TokenShare: 0.6},
		},
	}
	// maxPerAsset doc = 10*0.6/10 = 0 → floored to 1; same for memory. So each
	// candidate costs 1 token. 6 candidates (3 doc + 3 mem) = 6 tokens ≤ 10; the
	// 11th token's 7th... to force budget_full we need >10 candidates.
	cands := make([]ScoredCandidate, 0, 12)
	for i := 0; i < 6; i++ {
		cands = append(cands, ScoredCandidate{
			Candidate: KnowledgeCandidate{
				AssetID: id(docName(i) + "d"), AssetType: domain.AssetTypeDocument, Snippet: "x",
			},
			Score: 0.9 - float64(i)*0.01,
		})
	}
	for i := 0; i < 6; i++ {
		cands = append(cands, ScoredCandidate{
			Candidate: KnowledgeCandidate{
				AssetID: id(docName(i) + "m"), AssetType: domain.AssetTypeMemory, Snippet: "x",
			},
			Score: 0.5 - float64(i)*0.01,
		})
	}
	selected, trunc := b.Select(cands, budget)
	// 12 candidates × 1 token = 12 > MaxTokens 10 → 10 selected, 2 truncated.
	// Neither per-type cap fires (each type cap = 10*0.6 = 6 tokens; each type
	// has 6 candidates × 1 token = 6 ≤ 6, exactly at cap for the type that
	// fills). The headline reason is budget_full (the global cap is what stops
	// the 11th).
	if len(selected) > 10 {
		t.Errorf("selected = %d, want ≤ 10 (MaxTokens)", len(selected))
	}
	if trunc.Reason != TruncReasonBudgetFull {
		t.Errorf("Reason = %q, want budget_full (global cap stops before per-type)", trunc.Reason)
	}
	if len(trunc.TruncatedAssetIDs) == 0 {
		t.Error("expected truncation asset_ids, got none")
	}
}

func TestBudgeter_TypeNotInQuotaDroppedAndReported(t *testing.T) {
	// §6.1: a type with no quota entry is not allowed into the budget. It is
	// dropped and reported (continue tool skill_resources for a skill not in
	// the quota).
	b := NewBudgeter()
	budget := Budget{
		MaxTokens: 1000,
		MaxItems:  10,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 5, TokenShare: 1.0},
		},
	}
	cands := []ScoredCandidate{
		{Candidate: KnowledgeCandidate{AssetID: id("doc"), AssetType: domain.AssetTypeDocument, Snippet: "x"}, Score: 0.9},
		{Candidate: KnowledgeCandidate{AssetID: id("sk"), AssetType: domain.AssetTypeSkill, Snippet: "x"}, Score: 0.8},
	}
	selected, trunc := b.Select(cands, budget)
	if len(selected) != 1 {
		t.Errorf("selected = %d, want 1 (skill not in quota)", len(selected))
	}
	if selected[0].AssetType != domain.AssetTypeDocument {
		t.Errorf("wrong type selected: %v", selected[0].AssetType)
	}
	if trunc.Reason != TruncReasonQuotaExhausted {
		t.Errorf("Reason = %q, want quota_exhausted", trunc.Reason)
	}
	if !contains(trunc.ContinueTools, "skill_resources") {
		t.Errorf("ContinueTools = %v, want skill_resources", trunc.ContinueTools)
	}
}

func TestBudgeter_NoSilentTruncation_ReportCarriesTruncatedIDs(t *testing.T) {
	// §11.4 / DoD: no silent truncation. Every truncated candidate's asset_id
	// appears in TruncationReport.TruncatedAssetIDs (distinct), plus the
	// continue tools. Here 6 docs (MaxItems=5) → 1 truncated id.
	b := NewBudgeter()
	cands := make([]ScoredCandidate, 0, 6)
	for i := 0; i < 6; i++ {
		cands = append(cands, ScoredCandidate{
			Candidate: KnowledgeCandidate{
				AssetID: id(docName(i)), AssetType: domain.AssetTypeDocument,
				Snippet: "x",
			},
			Score: 0.9 - float64(i)*0.01,
		})
	}
	_, trunc := b.Select(cands, defaultBudget())
	if len(trunc.TruncatedAssetIDs) == 0 {
		t.Fatal("truncated but no asset_ids reported — silent truncation")
	}
	// Distinct.
	seen := map[string]bool{}
	for _, a := range trunc.TruncatedAssetIDs {
		if seen[a] {
			t.Errorf("duplicate truncated id %s", a)
		}
		seen[a] = true
	}
}

func TestBudgeter_ContinueToolsDistinctPerType(t *testing.T) {
	// §6.2: ContinueTools lists the per-type continue-read tools, distinct.
	// Truncate one doc + one skill (skill not in quota) → both tools present.
	b := NewBudgeter()
	budget := Budget{
		MaxTokens: 1000, MaxItems: 10,
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 1, TokenShare: 1.0}, // 1 doc allowed
		},
	}
	cands := []ScoredCandidate{
		{Candidate: KnowledgeCandidate{AssetID: id("d1"), AssetType: domain.AssetTypeDocument, Snippet: "x"}, Score: 0.9},
		{Candidate: KnowledgeCandidate{AssetID: id("d2"), AssetType: domain.AssetTypeDocument, Snippet: "x"}, Score: 0.8},
		{Candidate: KnowledgeCandidate{AssetID: id("s1"), AssetType: domain.AssetTypeSkill, Snippet: "x"}, Score: 0.7},
	}
	_, trunc := b.Select(cands, budget)
	if !contains(trunc.ContinueTools, "document_read") {
		t.Errorf("missing document_read: %v", trunc.ContinueTools)
	}
	if !contains(trunc.ContinueTools, "skill_resources") {
		t.Errorf("missing skill_resources: %v", trunc.ContinueTools)
	}
	// Distinct (no duplicate document_read).
	count := 0
	for _, t := range trunc.ContinueTools {
		if t == "document_read" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("document_read listed %d times, want 1 (distinct)", count)
	}
}

func TestBudgeter_EmptyScoredNoTruncation(t *testing.T) {
	b := NewBudgeter()
	selected, trunc := b.Select(nil, defaultBudget())
	if len(selected) != 0 {
		t.Errorf("got %d selected, want 0", len(selected))
	}
	if trunc.Reason != "" || len(trunc.TruncatedAssetIDs) != 0 {
		t.Errorf("empty input → non-empty report: %+v", trunc)
	}
}

func TestBudgeter_NoTokenLimitItemsOnly(t *testing.T) {
	// §6.1: MaxTokens=0 means no token limit; only MaxItems governs. 6 docs,
	// MaxItems unlimited (0) but doc quota MaxItems=5 → 1 truncated.
	b := NewBudgeter()
	budget := Budget{
		MaxTokens: 0, // no token budget
		MaxItems:  0, // no global item cap
		TypeQuota: map[domain.AssetType]Quota{
			domain.AssetTypeDocument: {MaxItems: 5, TokenShare: 0},
		},
	}
	cands := make([]ScoredCandidate, 0, 6)
	for i := 0; i < 6; i++ {
		cands = append(cands, ScoredCandidate{
			Candidate: KnowledgeCandidate{
				AssetID: id(docName(i)), AssetType: domain.AssetTypeDocument,
				Snippet: strings.Repeat("x", 100),
			},
			Score: 0.9 - float64(i)*0.01,
		})
	}
	selected, trunc := b.Select(cands, budget)
	if len(selected) != 5 {
		t.Errorf("selected = %d, want 5 (doc MaxItems, no token limit)", len(selected))
	}
	if trunc.Reason != TruncReasonQuotaExhausted {
		t.Errorf("Reason = %q, want quota_exhausted", trunc.Reason)
	}
}

func TestBudgeter_PreservesScoreOrder(t *testing.T) {
	// §5.2 / §6.2: the Budgeter iterates the policy-sorted list (highest first)
	// and selects in that order. The selected slice keeps the score order.
	b := NewBudgeter()
	cands := []ScoredCandidate{
		{Candidate: KnowledgeCandidate{AssetID: id("low"), AssetType: domain.AssetTypeDocument, Snippet: "x"}, Score: 0.1},
		{Candidate: KnowledgeCandidate{AssetID: id("high"), AssetType: domain.AssetTypeDocument, Snippet: "x"}, Score: 0.9},
		{Candidate: KnowledgeCandidate{AssetID: id("mid"), AssetType: domain.AssetTypeDocument, Snippet: "x"}, Score: 0.5},
	}
	// Sort cands by score desc to simulate the policy output.
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].Score > cands[i].Score {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	selected, _ := b.Select(cands, defaultBudget())
	if len(selected) != 3 {
		t.Fatalf("got %d, want 3", len(selected))
	}
	if selected[0].AssetID != id("high") || selected[2].AssetID != id("low") {
		t.Errorf("order not preserved: %v %v %v", selected[0].AssetID, selected[1].AssetID, selected[2].AssetID)
	}
}

// docName returns a legible distinct name for the i-th doc in a generated list.
func docName(i int) string {
	switch i {
	case 0:
		return "doc0"
	case 1:
		return "doc1"
	case 2:
		return "doc2"
	case 3:
		return "doc3"
	case 4:
		return "doc4"
	default:
		return "doc5"
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
