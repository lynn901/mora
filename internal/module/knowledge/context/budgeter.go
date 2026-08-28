// budgeter.go — Budget/Quota/TruncationReport 类型（§6.1）+ Budgeter 签名（§6.2）。
//
// The Budgeter selects candidates under a token + item + per-type quota budget
// (D6). It degrades in a ladder: catalog → summary → snippet → "read more"
// tool hint (§6.2), and it MUST NOT silently truncate citations (§6.2 / §11.4)
// — truncation returns the reason + the continue-read tool name. A single
// asset cannot exhaust the whole budget: each type's TokenShare is capped so a
// long document does not crowd out every other type (§6.2).
//
// Default behavior is catalog-first (no body); the agent re-reads body via the
// type-specific tools (§6.2 / §11.3). Budget source: caller MaxTokens/MaxItems,
// else workspace default (§6.3).

package contextbroker

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
	Reason            string    // quota_exhausted | budget_full | deadline
	TruncatedAssetIDs []uuid.UUID
	ContinueTools     []string // asset_read / code_node / memory_evidence_read / skill_resources
}

// Budgeter selects candidates under budget (§6.2). Implementations land in a
// follow-up sub-task; the signature is fixed here. Select MUST return a
// TruncationReport that, when it drops anything, carries the reason + the
// continue-read tool names (§6.2 / §11.4 — no silent citation truncation).
type Budgeter interface {
	Select(scored []ScoredCandidate, budget Budget) (selected []KnowledgeCandidate, truncation TruncationReport)
}
