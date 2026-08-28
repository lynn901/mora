// Package context — Budgeter (design-docs/19 §6, D6).
//
// The Budgeter trims the policy-scored candidate list to a token/item budget
// using a DEGRADATION LADDER: directory + summary + citation first, body by
// progressive read (§6.2, 12 §9.6 "默认先返回资产目录、摘要和引用"). It never
// lets one asset fill the budget (§6.2 per-type TokenShare cap) and never
// silently truncates a citation — when the budget runs out it returns a
// TruncationReport with the reason, the truncated asset_ids, and the
// continue-read tool names so the Agent can fetch the rest (§11.4 "上下文预算
// 不足 → 返回截断原因和继续读取工具，不静默截断引用").
//
// This file is the budget model + Select; the Broker (YS-209) calls Select as
// step 8 of the §7.1 pipeline, after Score (step 7) and before CitationBuilder
// (step 9).
package context

import (
	"time"

	"github.com/lynn901/mora/internal/domain"
)

// Budget is the budget model (§6.1). MaxTokens is the total token budget
// (from KnowledgeQuery.MaxTokens or the workspace default, §6.3). MaxItems is
// the total item cap. TypeQuota is the per-type cap (items + token share). The
// Timeout is the shared deadline; Budgeter.Select does NOT enforce the deadline
// itself (the parallel fan-out in §7.1 step 4 owns the ctx deadline) — the
// deadline field is carried so TruncationReport can name it when the Broker
// reports a deadline-driven truncation.
type Budget struct {
	MaxTokens int                        // total token budget; 0 = no token limit (items only)
	MaxItems  int                        // total item cap; 0 = no item limit
	TypeQuota map[domain.AssetType]Quota // per-type cap; a missing type = type not allowed
	Timeout   time.Duration             // shared deadline (default 2s, §6.1 / §14.3)
}

// Quota is the per-type cap (§6.1). MaxItems is the max items of this type.
// TokenShare is this type's token fraction in [0,1] — a SINGLE ASSET cannot
// fill the budget (12 §9.6), so TokenShare caps how much of MaxTokens one type
// may consume. The per-asset token cap is MaxTokens*TokenShare/MaxItems so one
// long document cannot squeeze out every other type.
type Quota struct {
	MaxItems   int
	TokenShare float64 // [0,1]; 0 = this type consumes no tokens (e.g. citation-only)
}

// TruncationReason is why the Budgeter stopped (§6.2). The Broker surfaces this
// in the response so the Agent knows the result is partial, not complete
// (§11.4 — never present a truncated result as if it were whole).
type TruncationReason string

const (
	// TruncReasonQuotaExhausted: a per-type quota (items or token share) was
	// reached before the budget was full (§6.2).
	TruncReasonQuotaExhausted TruncationReason = "quota_exhausted"
	// TruncReasonBudgetFull: the total token/item budget was reached (§6.2).
	TruncReasonBudgetFull TruncationReason = "budget_full"
	// TruncReasonDeadline: the shared deadline elapsed mid-select. The fan-out
	// (§7.1 step 4) owns the ctx; the Broker sets this when it observes a
	// deadline-driven truncation.
	TruncReasonDeadline TruncationReason = "deadline"
)

// TruncationReport records what was cut and how to continue (§6.2, §11.4).
// TruncatedAssetIDs are the candidates that scored in but did not fit the
// budget — the Agent can fetch them via ContinueTools. ContinueTools is the
// per-type continue-read tool names (document_read/code_node/
// memory_evidence_read/skill_resources), so the Agent knows WHICH tool to call
// for WHICH truncated asset. Empty when nothing was truncated.
type TruncationReport struct {
	Reason            TruncationReason `json:"reason,omitempty"`
	TruncatedAssetIDs []string         `json:"truncated_asset_ids,omitempty"`
	ContinueTools     []string         `json:"continue_tools,omitempty"`
}

// Budgeter applies the degradation ladder + per-type caps to the scored list
// (§6.2). Select is the only method; the Broker calls it after Score.
type Budgeter interface {
	Select(scored []ScoredCandidate, budget Budget) (selected []KnowledgeCandidate, truncation TruncationReport)
}

// continueTool maps an asset type to its progressive-read tool name (§6.2 "继续
// 读取工具提示"). The Agent calls these to fetch the body of a candidate the
// Budgeter left at directory+summary+citation. The names align with the §11.3
// example (document_read/code_node/...) and the type-specialized tools the
// Broker does NOT flatten (D12).
var continueTool = map[domain.AssetType]string{
	domain.AssetTypeDocument: "document_read",
	domain.AssetTypeCodebase: "code_node",
	domain.AssetTypeMemory:   "memory_evidence_read",
	domain.AssetTypeSkill:    "skill_resources",
}

// budgeter is the default Budgeter (§6.2).
type budgeter struct{}

// NewBudgeter builds the default Budgeter.
func NewBudgeter() Budgeter { return &budgeter{} }

// Select implements the degradation ladder (§6.2):
//
//  1. Iterate the policy-sorted scored list (highest score first).
//  2. Default: select at directory + summary + citation (no body). The body is
//     a progressive read via the continue tool (§6.2 / §11.4). IncludeContent
//     on the query (carried on each candidate's snippet budget) is not a
//     Budgeter concern here — the snippet IS the budgeted excerpt.
//  3. Accumulate tokens per candidate (estimateTokens), tracking per-type
//     counts and token spend. Stop a type when its MaxItems or TokenShare cap
//     is hit; stop globally when MaxTokens or MaxItems is hit.
//  4. A SINGLE ASSET cannot fill the budget (§6.2): the per-type TokenShare
//     caps the type, and a single candidate's tokens are capped at
//     maxPerAsset = MaxTokens*TokenShare/MaxItems so one long document cannot
//     squeeze out every other type.
//  5. Leftover scored candidates → TruncationReport with the reason, their
//     asset_ids, and the continue tool names (§11.4 — no silent truncation).
//
// Tokens are estimated (snippet length / 4, a conservative char→token ratio).
// The estimate is a budget guard, not a bill; the real spend is observed by the
// Broker and emitted as the knowledge_context_tokens metric (§9.2).
func (b *budgeter) Select(scored []ScoredCandidate, budget Budget) ([]KnowledgeCandidate, TruncationReport) {
	if len(scored) == 0 {
		return nil, TruncationReport{}
	}

	totalTokens := 0
	totalItems := 0
	typeItems := make(map[domain.AssetType]int, len(budget.TypeQuota))
	typeTokens := make(map[domain.AssetType]int, len(budget.TypeQuota))

	selected := make([]KnowledgeCandidate, 0, len(scored))
	var truncated []ScoredCandidate
	// Collect every distinct truncation reason that fired; the report carries
	// the highest-priority one (§6.2). Priority: deadline > budget_full >
	// quota_exhausted — the most global reason is the headline (when the total
	// budget is full AND a per-type cap also fired, "budget_full" is the
	// actionable headline; the per-type cap is implied).
	reasons := make(map[TruncationReason]bool)

	for _, sc := range scored {
		t := sc.Candidate.AssetType

		// A type not in TypeQuota is not allowed into the budget (§6.1 — the
		// Intent Router's type set should be a subset of TypeQuota's keys; a
		// type with no quota is dropped, not silently included).
		quota, allowed := budget.TypeQuota[t]
		if !allowed {
			truncated = append(truncated, sc)
			reasons[TruncReasonQuotaExhausted] = true
			continue
		}

		// Per-type item cap (§6.1 MaxItems).
		if quota.MaxItems > 0 && typeItems[t] >= quota.MaxItems {
			truncated = append(truncated, sc)
			reasons[TruncReasonQuotaExhausted] = true
			continue
		}

		// Total item cap (§6.1 MaxItems).
		if budget.MaxItems > 0 && totalItems >= budget.MaxItems {
			truncated = append(truncated, sc)
			reasons[TruncReasonBudgetFull] = true
			continue
		}

		// Token accounting. Single-asset cap: a candidate may consume at most
		// maxPerAsset tokens so one long asset cannot fill the budget (§6.2).
		// maxPerAsset = MaxTokens*TokenShare/MaxItems (>=1 so a tiny budget does
		// not zero out every candidate).
		tokens := estimateTokens(sc.Candidate.Snippet)
		maxPerAsset := 1
		if budget.MaxTokens > 0 && quota.MaxItems > 0 && quota.TokenShare > 0 {
			maxPerAsset = int(float64(budget.MaxTokens) * quota.TokenShare / float64(quota.MaxItems))
			if maxPerAsset < 1 {
				maxPerAsset = 1
			}
		}
		if tokens > maxPerAsset {
			tokens = maxPerAsset
		}

		// Per-type token cap (TokenShare of MaxTokens, §6.2).
		if budget.MaxTokens > 0 && quota.TokenShare > 0 {
			typeCap := int(float64(budget.MaxTokens) * quota.TokenShare)
			if typeTokens[t]+tokens > typeCap {
				truncated = append(truncated, sc)
				reasons[TruncReasonQuotaExhausted] = true
				continue
			}
		}

		// Total token cap (§6.1 MaxTokens).
		if budget.MaxTokens > 0 && totalTokens+tokens > budget.MaxTokens {
			truncated = append(truncated, sc)
			reasons[TruncReasonBudgetFull] = true
			continue
		}

		// The candidate fits: select it (directory + summary + citation — the
		// body is NOT inlined; §6.2). The CitationBuilder (§8) completes the
		// citation after this step.
		selected = append(selected, sc.Candidate)
		totalTokens += tokens
		totalItems++
		typeItems[t]++
		typeTokens[t] += tokens
	}

	// No truncation → empty report (the response omits the truncation field,
	// §11.3). Any truncation → report with the highest-priority reason +
	// asset_ids + continue tools.
	if len(truncated) == 0 {
		return selected, TruncationReport{}
	}
	report := TruncationReport{
		Reason:            pickReason(reasons),
		TruncatedAssetIDs: make([]string, 0, len(truncated)),
		ContinueTools:     continueToolsFor(truncated),
	}
	seen := make(map[string]bool, len(truncated))
	for _, sc := range truncated {
		id := sc.Candidate.AssetID.String()
		if seen[id] {
			continue
		}
		seen[id] = true
		report.TruncatedAssetIDs = append(report.TruncatedAssetIDs, id)
	}
	return selected, report
}

// continueToolsFor returns the distinct continue-read tool names for the
// truncated candidates' types (§6.2 / §11.4). The Agent uses these to fetch
// the bodies of the truncated assets. Distinct so the report is compact.
func continueToolsFor(truncated []ScoredCandidate) []string {
	seen := make(map[string]bool, len(truncated))
	out := make([]string, 0, len(truncated))
	for _, sc := range truncated {
		tool, ok := continueTool[sc.Candidate.AssetType]
		if !ok || seen[tool] {
			continue
		}
		seen[tool] = true
		out = append(out, tool)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// estimateTokens is a conservative char→token estimate (§6.2 token accounting).
// 4 chars/token is the standard upper-bound ratio for mixed CJK+Latin text;
// CJK-heavy snippets over-estimate slightly, which is safe for a budget guard
// (better to truncate early than to over-spend). The real spend is observed by
// the Broker, not by this estimate.
func estimateTokens(snippet string) int {
	if len(snippet) == 0 {
		return 1 // a citation-only entry still costs a floor of 1 token
	}
	n := len(snippet) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// pickReason returns the highest-priority truncation reason that fired (§6.2).
// Priority: deadline > budget_full > quota_exhausted — the most global reason
// is the headline. When both a per-type cap and the total budget are hit,
// "budget_full" is the actionable headline (the per-type cap is implied).
// An empty set (no reasons) yields "" — but pickReason is only called when
// truncation occurred, so the set is non-empty in practice.
func pickReason(reasons map[TruncationReason]bool) TruncationReason {
	// The deadline is observed by the Broker (§7.1 step 4 owns the ctx); if it
	// sets the deadline reason, that dominates (a deadline-driven truncation is
	// not a budget decision).
	if reasons[TruncReasonDeadline] {
		return TruncReasonDeadline
	}
	if reasons[TruncReasonBudgetFull] {
		return TruncReasonBudgetFull
	}
	if reasons[TruncReasonQuotaExhausted] {
		return TruncReasonQuotaExhausted
	}
	return TruncReasonBudgetFull // safe default
}
