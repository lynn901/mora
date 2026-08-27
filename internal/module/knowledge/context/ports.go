// Package contextbroker — typed query ports (design-docs/19 §3.3, doc 12 §9.4).
//
// The four type-query ports are the seam the Broker orchestrates. Each port
// returns the unified KnowledgeCandidate (candidate.go) so the Broker ranks,
// dedups, and cites one shape regardless of source type. Ports preserve the
// type-specialized engines they adapt — DocumentQuery is not a generic Search,
// CodeQuery keeps code semantics, etc. (§0 D3 / D12: do not flatten specialized
// tools).
//
// Authorization invariant (§3.2): a Provider/port adapter MUST NOT accept a
// user-submitted allowed_asset_ids. It accepts only the server-built
// platform/authz.AuthzContext (pushed down as a hard pre-filter, §0 D10). The
// Broker also runs a batch post-check after the fetch — the port's own
// hard-filter is necessary but not the sole gate.
package contextbroker

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/authz"
)

// KnowledgeQuery is the unified Broker input (doc 12 §9.1, design-docs/19
// §11.3 request). The Broker resolves principal + AuthzContext (step 1-2) and
// intent + asset types (step 3) BEFORE fanning out to ports; the per-type
// request structs below carry the narrowed scope each port needs.
type KnowledgeQuery struct {
	Query       string         // free-text; drives each port's own ranking
	WorkspaceID uuid.UUID
	AgentID     *uuid.UUID    // present when an agent is the principal
	// AssetTypes is the caller's explicit type set; empty = IntentRouter picks
	// (§4.2 routing rule 1). The Broker resolves this before fanning out.
	AssetTypes []domain.AssetType
	// Filters narrow per-type scopes (directory_id, memory_type, code path,
	// skill delivery_mode, …). Opaque here; each port reads its own keys.
	Filters map[string]any
	MaxTokens int            // budget; 0 = workspace default (§6.3)
	MaxItems  int            // item cap; 0 = port default
	Timeout   time.Duration  // shared deadline (default 2s, §6.1 / §14.3)
	// IncludeContent: false (default) = directory + summary + citation only
	// (§6.2). The body is a progressive read via the type tool, not inlined.
	IncludeContent bool
}

// AuthzContext is re-exported from platform/authz so port signatures name the
// canonical type the Broker pushes down. It carries DecisionID +
// AllowedAssetIDs (server-resolved) + AuthzRevision (cache key, §0 D10).
// Aliased (not copied) to stay 1:1 with platform/authz evolution.
type AuthzContext = authz.AuthzContext

// DocumentQuery is the document type port (12 §9.4). Its adapter wraps
// mora/search.SearchExecutor + rag/search.HybridSearcher and maps SearchHit /
// search.Result into the unified candidate, carrying version_id as the version
// anchor. RBAC is a hard filter pushed from AuthzContext (§3.2).
type DocumentQuery interface {
	Search(ctx context.Context, ac AuthzContext, q DocumentQueryRequest) ([]KnowledgeCandidate, error)
}

// DocumentQueryRequest is the narrowed document scope (directory / tags /
// updated window / pagination). The Broker fills it from KnowledgeQuery +
// AuthzContext.AllowedAssetIDs.
type DocumentQueryRequest struct {
	Query       string
	WorkspaceID uuid.UUID
	DirectoryID *uuid.UUID
	Tags        []uuid.UUID
	UpdatedAfter  *time.Time
	UpdatedBefore *time.Time
	MaxItems    int
}

// CodeQuery is the codebase type port (12 §9.4). Its adapter wraps
// codegraph/service read-only queries (search/explore) and maps CodeHit into
// the unified candidate, carrying commit + source_tree_ref as the version
// (§3.3). Read-only — it never triggers a build (§3.2).
type CodeQuery interface {
	Search(ctx context.Context, ac AuthzContext, q CodeQueryRequest) ([]KnowledgeCandidate, error)
}

// CodeQueryRequest names the codebase asset + the search/explore axis.
type CodeQueryRequest struct {
	Query     string
	AssetID   uuid.UUID // the codebase knowledge_asset to query
	Language  string
	PathGlob  string
	MaxItems  int
}

// MemoryQuery is the memory type port (12 §9.4). It is implemented by the
// existing recall.RecallService (§3.3 — reuse, not re-implement); the adapter
// converts recall.KnowledgeCandidate into the unified shape (candidate.go
// CandidateFromMemory). Leak-safe (§9.3): an unauthorized caller gets empty,
// never a 403/404 distinction.
type MemoryQuery interface {
	Recall(ctx context.Context, ac AuthzContext, q MemoryQueryRequest) ([]KnowledgeCandidate, error)
}

// MemoryQueryRequest is the narrowed memory scope (owner / memory_type /
// valid-at / linked asset). Mirrors recall.KnowledgeQuery axes (§8.1) but as
// the Broker-facing request; the adapter translates into recall.KnowledgeQuery.
type MemoryQueryRequest struct {
	Query           string
	WorkspaceID     uuid.UUID
	OwnerID         *uuid.UUID
	MemoryType      *string
	ValidAt         *time.Time
	AssetID         *uuid.UUID
	IncludeCandidates bool // owner-only / review-view (§8.5); adapter honors ONLY for owner
	MaxItems        int
}

// SkillQuery is the skill type port (12 §9.4). Its adapter wraps
// skill/delivery ArchiveReader and returns a candidate trimmed by the binding
// delivery_mode (tool=SKILL.md head, summary=description, inline=resource
// list), carrying package_version as the version (§3.3).
type SkillQuery interface {
	Discover(ctx context.Context, ac AuthzContext, q SkillQueryRequest) ([]KnowledgeCandidate, error)
}

// SkillQueryRequest names the skill(s) to discover. AssetIDs empty = list the
// agent's allowed skills (binding-resolved); a single id delivers one skill.
type SkillQueryRequest struct {
	Query      string
	WorkspaceID uuid.UUID
	AgentID    uuid.UUID // required: skill delivery is agent-bound (§6.2)
	AssetIDs   []uuid.UUID // empty = list allowed skills for the agent
	MaxItems   int
}
