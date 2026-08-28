// Package contextbroker implements the Phase 6 Context Broker
// (design-docs/19-phase6-context-broker.md). It is the orchestration layer that
// routes a knowledge query to the right intent + authority policy, fans out
// across the four type-query ports in parallel under a shared deadline,
// deduplicates while preserving conflicts, budgets the result, and builds
// traceable citations — all behind the two-stage authorization gate
// (platform/authz Authorize before, VisibleAssets batch post-check after).
//
// This file lands the module skeleton + type definitions only (YS-206). The
// orchestration logic, port adapters, and handler implementations land in
// follow-up sub-tasks. The package name is contextbroker (not context) to avoid
// shadowing the standard library "context" import at every call site — same
// domain-name-not-dir-name precedent as internal/module/knowledge/codegraph.
package contextbroker

// intent.go — Intent 枚举（§4.1）+ IntentRouter 端口（§4.3）。
//
// The Intent selects the authority policy (12 §9.5). Four built-in intents map
// 1:1 to the four built-in authority policies (§5.1). First-version routing is
// rule-based (§4.2); model-based classification is a §10 open decision and is
// NOT implemented here.

import (
	"context"

	"github.com/lynn901/mora/internal/domain"
)

// Intent is the query intent that selects the authority policy (12 §9.5).
// Four built-in intents map 1:1 to the four built-in authority policies.
type Intent string

const (
	// IntentSpec — 规范要求：当前有效且经治理批准的文档。
	IntentSpec Intent = "spec"
	// IntentRevision — revision 实现：固定 commit 的代码/配置/迁移/测试。
	IntentRevision Intent = "revision"
	// IntentRationale — 决策原因：决策文档、审核 Memory 与证据。
	IntentRationale Intent = "rationale"
	// IntentProcedure — 执行流程：已批准 Skill、Runbook、环境约束。
	IntentProcedure Intent = "procedure"
)

// Valid reports whether i is one of the four built-in intents. Used by the
// policy loader + intent router to reject an unknown intent early rather than
// silently falling back to a default (the DB CHECK constraint mirrors this on
// the config side, §2.1 chk_authority_intent).
func (i Intent) Valid() bool {
	switch i {
	case IntentSpec, IntentRevision, IntentRationale, IntentProcedure:
		return true
	}
	return false
}

// IntentRouter selects the query intent and target asset-type set (12 §9.2
// step 3). First version uses rule-based routing; model-based classification
// is deferred (§10).
//
// Route decides ONLY the policy + asset-type set — never authorization
// (authorization is computed independently by authz.Service, §4.2). The
// keyword tables are part of the versioned policy config (§5), PM-tunable, not
// hardcoded in the router logic.
type IntentRouter interface {
	Route(ctx context.Context, q KnowledgeQuery) (Intent, []domain.AssetType, error)
}
