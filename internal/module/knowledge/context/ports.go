// ports.go — 四类型查询端口接口（DocumentQuery/CodeQuery/MemoryQuery/SkillQuery，
// §3.3）+ AuthzContext + KnowledgeQuery（broker-level，对齐 §11.3 请求 shape）。
//
// Per-type adapters wrap the native type-engine queries and return the unified
// KnowledgeCandidate (D2). Adapters accept ONLY the server-constructed
// AuthzContext — never a user-submitted allowed_asset_ids (§3.2: "Provider
// adapter 不接受用户提交的 allowed_asset_ids，只接受 Mora 服务端构造的
// AuthzContext"). Authorization is computed by platform/authz, not by the
// ports: Authorize before the parallel fan-out (§7.1 step 1-2), VisibleAssets
// batch post-check after (§7.1 step 5).
//
// NOTE on AuthzContext: the broker carries the platform/authz.AuthzContext so
// the two-stage gate (D10) is enforced through the same decision pipeline the
// rest of the platform uses. To avoid a hard import cycle (knowledge/context →
// platform/authz → ... — authz must not depend on a module above it), the
// broker takes a narrow local AuthzContext the wiring layer (service.go) maps
// from platform/authz.AuthzContext. The fields mirror authz.AuthzContext 1:1
// (§3.3 seam); the mapping is a pure copy, no re-derivation.

package contextbroker

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// AuthzContext carries the resolved authorization for a broker request (mirrors
// platform/authz.AuthzContext 1:1, §3.3 seam). AllowedAssetIDs empty means
// "workspace-level visible, resolved server-side via decision_id" — same
// semantics as authz.AuthzContext, NOT "no assets".
type AuthzContext struct {
	WorkspaceID     uuid.UUID
	AuthzRevision   int64
	PrincipalType   domain.SubjectType
	PrincipalID     uuid.UUID
	ActingUserID    *uuid.UUID // present when an agent acts on behalf of a user
	AgentID         *uuid.UUID // present when principal_type=agent
	Allowed         bool
	Reason          string
	AllowedAssetIDs []uuid.UUID // empty = workspace-level (server-resolved)
	DecisionID      *uuid.UUID  // present when issued as a signed capability
}

// KnowledgeQuery is the broker-level query input (aligned with the §11.3
// request shape). It is distinct from recall.KnowledgeQuery (the memory-
// surface-specific input, design-doc 18 §9.1) — the broker's is the superset
// the IntentRouter routes on and the Budgeter budgets against. The memory
// adapter narrows this to recall.KnowledgeQuery when it calls RecallService
// (§3.3 seam); the narrowing is an adapter concern, not a type identity.
type KnowledgeQuery struct {
	Query       string // free-text; drives the type ports + intent routing
	WorkspaceID uuid.UUID
	AgentID     *uuid.UUID // optional; present for agent-driven context calls
	// AssetTypes narrows the fan-out. Empty = IntentRouter decides (§4.2).
	AssetTypes []domain.AssetType
	// Filters narrow the directory / owner / time / linked-asset axes (§11.3).
	Filters map[string]any
	// MaxTokens / MaxItems are the caller-supplied budget (§6.3). Zero = use
	// the workspace default (the Budgeter resolves the effective budget).
	MaxTokens int
	MaxItems  int
	// Timeout is the shared fan-out deadline (§7.1). Zero = default 2s
	// (12 §14.3 SLO). The broker uses min(q.Timeout, 2s).
	Timeout time.Duration
	// IncludeContent opts into returning body, not just catalog+summary+citation
	// (§11.3, default false). Default behavior: the agent re-reads via the
	// type-specific tools (§6.2).
	IncludeContent bool
}

// DocumentQuery is the document port (§3.3). The adapter wraps
// mora/search.SearchExecutor + rag/search.HybridSearcher, mapping SearchHit /
// search.Result to KnowledgeCandidate. RBAC hard-filter is unchanged (§13).
type DocumentQuery interface {
	Query(ctx context.Context, auth AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error)
}

// CodeQuery is the codebase port (§3.3). The adapter wraps codegraph/service
// read-only search/explore, mapping CodeHit to KnowledgeCandidate and carrying
// commit / source_tree_ref as VersionOrRevision (§8.1).
type CodeQuery interface {
	Query(ctx context.Context, auth AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error)
}

// MemoryQuery is the memory port (§3.3). The adapter wraps
// recall.RecallService.Recall, converging recall.KnowledgeCandidate to the
// unified shape WITHOUT breaking the Phase 4 recall contract (D2 / §13).
type MemoryQuery interface {
	Query(ctx context.Context, auth AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error)
}

// SkillQuery is the skill port (§3.3). The adapter wraps skill/delivery.go
// ArchiveReader, returning candidates per binding delivery_mode (tool =
// SKILL.md head; summary = description; inline = resource list).
type SkillQuery interface {
	Query(ctx context.Context, auth AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error)
}
