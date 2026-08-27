// broker.go — ContextBroker 接口 + Execute 签名（§7.1 十步流水线骨架）。
//
// The ContextBroker is the Phase 6 orchestration layer (§7.1). Execute runs
// the ten-step pipeline:
//
//	step 1-2: 解析 principal + authz.Authorize → AuthzContext(decision_id,
//	          AllowedAssetIDs, revision)   [§7.1 step 1-2]
//	step 3:   IntentRouter.Route → (Intent, assetTypes)        [§7.1 step 3]
//	step 4:   并发调用各类型端口（errgroup + 共同 deadline =
//	          min(q.Timeout, 2s)），每端口返回 []KnowledgeCandidate 或
//	          (error, degraded_reason)                          [§7.1 step 4]
//	step 5:   合并所有 candidate ID → authz 批量 post-check（VisibleAssets）
//	          → 允许子集                                        [§7.1 step 5]
//	step 6:   DedupAndKeepConflicts                           [§7.2 / step 6]
//	step 7:   AuthorityPolicy.Score（按 Intent 加载策略）       [§7.1 step 7]
//	step 8:   Budgeter.Select                                  [§6.2 / step 8]
//	step 9:   CitationBuilder.Build                            [§8  / step 9]
//	step 10:  审计摘要（入选/淘汰原因 + policy_version +
//	          authz_revision）                                  [§7.1 step 10]
//
// Invariants (附录 A #16-#21): the broker never bypasses platform/authz
// (two-stage gate); cross-type dedup keeps conflicts; the three empty states
// are distinguishable (D8); truncation returns reason + continue-read tool;
// the policy is versioned; the cache key carries authz revision + asset
// version + policy version.
//
// This file lands the interface + the ten-step skeleton as TODO stubs. The
// orchestration logic (errgroup fan-out, post-check, scoring, budgeting,
// citation, audit) lands in a follow-up sub-task.

package contextbroker

import (
	"context"

	"github.com/google/uuid"
)

// ContextBroker is the Phase 6 orchestration entry point (§7.1). Execute runs
// the ten-step pipeline on a KnowledgeQuery and returns a ContextResponse
// whose three empty-result states are distinguishable (D8).
type ContextBroker interface {
	Execute(ctx context.Context, q KnowledgeQuery) (ContextResponse, error)
}

// broker is the default ContextBroker implementation. It composes the four
// type-query ports, the IntentRouter, the AuthorityPolicy loader, the
// Budgeter, the CitationBuilder, and the authz.Service seam. Fields are
// populated by the wiring layer (service.go). Methods (Execute) land in a
// follow-up sub-task; the struct is declared here so the wiring compiles.
type broker struct {
	doc     DocumentQuery
	code    CodeQuery
	mem     MemoryQuery
	skill   SkillQuery
	router  IntentRouter
	budget  Budgeter
	cite    CitationBuilder
	authz   AuthzSeam
}

// Execute is the ten-step pipeline (§7.1). Skeleton only — the real fan-out /
// post-check / scoring / budgeting / citation / audit land in a follow-up
// sub-task. Returns a zero-value ContextResponse so callers compile today.
func (b *broker) Execute(ctx context.Context, q KnowledgeQuery) (ContextResponse, error) {
	// TODO: §7.1 ten-step pipeline.
	//   1-2. authz.Authorize → AuthzContext (decision_id, AllowedAssetIDs, rev)
	//   3.   router.Route(q) → (Intent, assetTypes)
	//   4.   errgroup fan-out (deadline = min(q.Timeout, 2s)); per-port failure
	//        → DegradedSources, does not block other ports (§9.6).
	//   5.   authz.VisibleAssets batch post-check → allowed subset; set
	//        authzVisibleRemoved when a non-empty pre-authz set narrows to empty.
	//   6.   DedupAndKeepConflicts (§7.2).
	//   7.   policy.Score (load by Intent, §5.3).
	//   8.   budget.Select (§6.2 — no silent citation truncation).
	//   9.   cite.Build (§8.2 — post-authz field mapping, no re-resolution).
	//   10.  audit summary (intent/policy_version/authz_revision/decision_id/
	//        candidate_count/selected_count/truncated_count/degraded/duration).
	_ = ctx
	_ = q
	return ContextResponse{}, nil
}

// AuthzSeam is the narrow authorization seam the broker depends on (§3.3 /
// §7.1 step 1-2 + step 5). It is a local port so the broker does NOT import
// platform/authz directly (avoiding a knowledge/context → platform/authz
// dependency that would invert the layering: authz must stay below the modules).
// The wiring layer (service.go) adapts the real authz.Service to this seam.
//
// Authorize runs the pre-check (decision_id + AllowedAssetIDs + revision);
// VisibleAssets runs the batch post-check on the merged candidate IDs. Both
// map 1:1 to platform/authz.Service.Authorize / .VisibleAssets (§3.3).
type AuthzSeam interface {
	Authorize(ctx context.Context, q KnowledgeQuery) (AuthzContext, error)
	VisibleAssets(ctx context.Context, auth AuthzContext, candidateAssetIDs []uuid.UUID) ([]uuid.UUID, error)
}

var _ ContextBroker = (*broker)(nil)
