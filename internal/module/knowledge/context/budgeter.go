package contextbroker

// budgeter.go — Budget/Quota/TruncationReport 类型（§6.1）+ Budgeter 实现（§6.2）。
//
// The Budgeter selects candidates under a token + item + per-type quota budget
// (D6). It degrades in a ladder: catalog → summary → snippet → "read more"
// tool hint (§6.2), and it MUST NOT silently truncate citations (§6.2 / §11.4)
// — truncation returns the reason + the continue-read tool name. A single
// asset cannot exhaust the whole budget: each type's TokenShare is capped so a
// long document does not crowd out every other type (§6.2 "单资产不能占满预算").
//
// Default behavior is catalog-first (no body); the agent re-reads body via the
// type-specific tools (§6.2 / §11.3). Budget source: caller MaxTokens/MaxItems,
// else workspace default (§6.3).

import (
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// Budget is the effective budget the Broker computes for a query (§6.1).
// MaxTokens comes from KnowledgeQuery.MaxTokens or the workspace default
// (§6.3); MaxItems bounds the total result count; TypeQuota caps each type;
// Timeout is the shared fan-out deadline (default 2s, 12 §14.3 SLO).
type Budget struct {
	MaxTokens int                       // total token budget (caller or workspace default)
	MaxItems  int                       // total item cap
	TypeQuota map[domain.AssetType]Quota // per-type quota (items + token share)
	Timeout   time.Duration             // shared fan-out deadline (default 2s)
}

// Quota is the per-asset-type budget slice (§6.1). MaxItems bounds how many of
// that type enter the result; TokenShare is that type's share of the total
// token budget (0..1) — a single asset cannot fill the whole budget (§6.2
// "单资产不能占满预算").
type Quota struct {
	MaxItems   int
	TokenShare float64 // 0..1; capped so one type cannot exhaust the budget
}

// TruncationReport records what the Budgeter dropped and why (§6.2). The broker
// returns this in the response so the caller knows truncation happened + has a
// continue-read tool to fetch the dropped candidates (§11.3 `truncation`).
// Silently dropping citations is forbidden (§11.4) — a non-empty dropped set
// MUST carry a reason + continue_tools.
type TruncationReport struct {
	Reason             string    // quota_exhausted | budget_full | deadline
	TruncatedAssetIDs  []uuid.UUID
	ContinueTools      []string // asset_read / code_node / memory_evidence_read / skill_resources
}

// Truncation reasons (§6.2). The report carries exactly one; the Budgeter picks
// the first that applies in ladder order: deadline (if the ctx already expired
// — though the Budgeter itself does not fan out, the broker may hand it a
// deadline-expired state) → quota_exhausted (a per-type quota filled) →
// budget_full (the total token/item budget filled).
const (
	TruncReasonQuotaExhausted = "quota_exhausted"
	TruncReasonBudgetFull     = "budget_full"
	TruncReasonDeadline       = "deadline"
)

// continueToolFor maps each asset type to the continue-read tool name the
// truncation report points the agent at (§6.2 step 4 / §11.3). These are the
// type-specific progressive-read tools — the body is NOT inlined; the agent
// re-reads it via these.
var continueToolFor = map[domain.AssetType]string{
	domain.AssetTypeDocument: "document_read",
	domain.AssetTypeCodebase: "code_node",
	domain.AssetTypeMemory:   "memory_evidence_read",
	domain.AssetTypeSkill:    "skill_resources",
}

// DefaultBudget is the workspace default the Broker uses when the caller omits
// MaxTokens/MaxItems (§6.3). PM-governed in YS-212; the values here are the
// architecture's first-version defaults. MaxTokens is a conservative catalog
// budget (no bodies); TypeQuota caps each type so no single type dominates.
//
// TokenShare per type sums to 1.0 and each is < 1.0 — the "单资产不能占满预算"
// cap (§6.2). The split favors the spec-intent primary basis (document+memory)
// since the default fallback intent is spec, but the broker recomputes the
// quota from the routed intent's primary basis in a later step; this default
// is only the zero-value fallback.
var DefaultBudget = Budget{
	MaxTokens: 4000,
	MaxItems:  20,
	TypeQuota: map[domain.AssetType]Quota{
		domain.AssetTypeDocument: {MaxItems: 8, TokenShare: 0.4},
		domain.AssetTypeCodebase: {MaxItems: 4, TokenShare: 0.2},
		domain.AssetTypeMemory:   {MaxItems: 6, TokenShare: 0.3},
		domain.AssetTypeSkill:     {MaxItems: 2, TokenShare: 0.1},
	},
	Timeout: 2 * time.Second,
}

// tokenEstimate approximates the token cost of a candidate's catalog entry
// (title + snippet + citation), NOT the body (§6.2 default = no body). This is
// a rough heuristic — the real tokenizer is the engine's; the Budgeter only
// needs a consistent monotonic estimate for budgeting, not a precise count.
// The 4-chars-per-token rule of thumb + a fixed citation overhead matches the
// recall module's existing estimate convention.
const (
	tokensPerChar     = 4
	citationOverhead  = 16 // locator + version anchor + source ref, roughly
	minTokensPerEntry = 8 // floor so a tiny title still counts against quota
)

// estimateTokens approximates the token cost of admitting one candidate's
// catalog entry (title + snippet + citation metadata). It never estimates the
// body — §6.2 default is catalog-first, body is a progressive read. The floor
// prevents a zero-length title from being "free" (every entry costs at least
// the citation overhead).
func estimateTokens(c KnowledgeCandidate) int {
	n := len(c.Title) + len(c.Snippet)
	return max2(minTokensPerEntry, n/tokensPerChar+citationOverhead)
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Budgeter selects candidates under budget (§6.2). The default implementation
// (LadderBudgeter) walks the policy-sorted candidate list in order, admitting
// each to the catalog (title + snippet + citation, no body) until either the
// per-type quota OR the total budget fills. Remaining candidates get a
// TruncationReport with the reason + their continue-read tool.
//
// Select MUST return a TruncationReport that, when it drops anything, carries
// the reason + the continue-read tool names (§6.2 / §11.4 — no silent citation
// truncation). The report's ContinueTools is the de-duplicated set of tools for
// the dropped candidates' types — the agent has one tool name per type, not one
// per dropped asset.
type Budgeter interface {
	Select(scored []ScoredCandidate, budget Budget) (selected []KnowledgeCandidate, truncation TruncationReport)
}

// LadderBudgeter is the default Budgeter (§6.2). It is stateless; Select is
// safe to call concurrently. It does NOT fan out (the broker does that in step
// 4); it only ranks-then-trims the already-fetched, already-scored candidates.
type LadderBudgeter struct{}

// NewLadderBudgeter returns the default §6.2 budgeter.
func NewLadderBudgeter() *LadderBudgeter { return &LadderBudgeter{} }

// Select implements the §6.2 degradation ladder:
//
//  1. Walk scored candidates in policy-rank order (caller pre-sorts via
//     AuthorityPolicy.Score; Select does not re-sort — §6.2 step 1 assumes the
//     list is already ranked).
//  2. Admit each as catalog (title + snippet + citation, NO body — §6.2
//     default) until per-type quota OR total token/item budget fills.
//  3. Every dropped candidate contributes to TruncationReport with its type's
//     continue-read tool; no citation is silently dropped (§11.4).
//
// A per-type running counter enforces the "单资产不能占满预算" cap: once a type's
// MaxItems OR its TokenShare of the total budget is consumed, further candidates
// of that type are dropped (quota_exhausted), but OTHER types keep admitting
// until their own quota or the total budget fills (budget_full).
func (b *LadderBudgeter) Select(scored []ScoredCandidate, budget Budget) ([]KnowledgeCandidate, TruncationReport) {
	if len(scored) == 0 {
		return nil, TruncationReport{}
	}

	maxTokens := budget.MaxTokens
	maxItems := budget.MaxItems
	if maxTokens <= 0 {
		maxTokens = DefaultBudget.MaxTokens
	}
	if maxItems <= 0 {
		maxItems = DefaultBudget.MaxItems
	}

	type typeUsage struct {
		items  int
		tokens int
	}
	quota := budget.TypeQuota
	if quota == nil {
		quota = DefaultBudget.TypeQuota
	}
	usage := make(map[domain.AssetType]*typeUsage, len(quota))
	for t := range quota {
		usage[t] = &typeUsage{}
	}

	var selected []KnowledgeCandidate
	var dropped []uuid.UUID
	continueTools := make(map[string]struct{})
	totalTokens := 0
	totalItems := 0
	quotaHit := false

	for _, sc := range scored {
		c := sc.Candidate
		cost := estimateTokens(c)

		// §6.2 per-type quota checks fire BEFORE the total caps so the reason
		// reflects the first binding constraint in ladder order: a candidate
		// dropped because its type's quota filled reports quota_exhausted, even
		// if the total budget is also near full (§6.2 "达到该类型 quota 或总预
		// 算时停止" — the type quota is checked first).
		//
		// Checks are PROJECTIVE (would admitting this candidate exceed the
		// cap?) not reactive (has the cap already been hit?) — a reactive
		// check would over-admit one candidate past the cap on the boundary.
		u, known := usage[c.AssetType]
		if !known {
			// A type with no explicit quota: admit up to the global caps only
			// (treat as unlimited per-type, still bound by total budget). This
			// is fail-open for types the broker did not quota (e.g. a new type
			// mid-rollout); the total caps still bound the result.
			u = &typeUsage{}
			usage[c.AssetType] = u
		} else {
			q := quota[c.AssetType]
			// Per-type item cap.
			if q.MaxItems > 0 && u.items+1 > q.MaxItems {
				dropped = append(dropped, c.AssetID)
				recordTool(continueTools, c.AssetType)
				quotaHit = true
				continue
			}
			// Per-type token cap (TokenShare * MaxTokens) — the "单资产不能占满
			// 预算" guard (§6.2). A type cannot consume more than its share even
			// if its candidates individually are cheap. Projective: would this
			// candidate push the type's token total past its share?
			typeTokenCap := int(q.TokenShare * float64(maxTokens))
			if typeTokenCap > 0 && u.tokens+cost > typeTokenCap {
				dropped = append(dropped, c.AssetID)
				recordTool(continueTools, c.AssetType)
				quotaHit = true
				continue
			}
		}

		// Total item cap (§6.1 MaxItems) — global backstop after the per-type
		// quota, so a type that has NOT filled its quota still stops when the
		// overall result is full. Projective.
		if totalItems+1 > maxItems {
			dropped = append(dropped, c.AssetID)
			recordTool(continueTools, c.AssetType)
			continue
		}
		// Total token cap (§6.1 MaxTokens). Projective: would this candidate
		// push the total past the budget?
		if totalTokens+cost > maxTokens {
			dropped = append(dropped, c.AssetID)
			recordTool(continueTools, c.AssetType)
			continue
		}

		// Admit the candidate's catalog entry (no body — §6.2 default).
		selected = append(selected, c)
		totalTokens += cost
		totalItems++
		u.items++
		u.tokens += cost
	}

	report := TruncationReport{
		TruncatedAssetIDs: dropped,
		ContinueTools:      toolSetToSlice(continueTools),
	}
	// §6.2 reason in ladder order: quota_exhausted (a per-type cap filled first)
	// takes precedence over budget_full (the total filled). deadline is set by
	// the broker when the fan-out deadline expired, not by the Budgeter — left
	// empty here when the Budgeter alone ran.
	switch {
	case len(dropped) == 0:
		// nothing dropped → empty report (no spurious reason)
	case quotaHit:
		report.Reason = TruncReasonQuotaExhausted
	default:
		report.Reason = TruncReasonBudgetFull
	}
	return selected, report
}

// drop appends a candidate's id + its type's continue-read tool to the report
// accumulators. Kept as helpers so the admit/drop branches read cleanly.
func recordTool(tools map[string]struct{}, t domain.AssetType) {
	if name, ok := continueToolFor[t]; ok {
		tools[name] = struct{}{}
	}
}

func toolSetToSlice(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
