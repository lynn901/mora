// Package binding implements the Agent 配装（Binding）management application
// service (design-docs/19 §5 — Phase 5-2, YS-162). It owns the management plane
// for agent_bindings: batch create/update/revoke, pinned-version governance
// alerting, delivery_mode resolution, and the agent.binding_changed audit
// event. It does NOT change the agent_bindings table structure (migration 022
// already added the supplemental indexes); it only adds application-layer
// logic over the existing columns.
//
// Layering (modular monolith): this service declares the storage ports;
// implementations live in internal/infra/postgres. The service stays pgx-free
// — transactions and the outbox double-write are owned by the Sink port
// (same precedent as source/service.SyncRunSink).
//
// Security invariants (§1.2 硬边界):
//   - Binding only NARROWS an agent's reachable set, never grants the acting
//     principal a capability it lacks (Phase 0 不变量 A #4). The authz
//     Service enforces the intersection; this service never widens effect.
//   - 固定版本不可静默漂移 (§5.1): a pinned binding whose pinned_version_id
//     is revoked/missing MUST block use — no auto-fallback to the latest
//     published version. The authz pinnedVersionGate enforces the block at
//     decision time; this service ALERTS (returns the binding in a blocked
//     state) when a pinned version is detected as no-longer-usable so callers
//     can surface the alert (§9 门禁: 阻断 + 告警).
//   - 存在性不泄露: a binding read for an agent the caller cannot see is
//     surfaced as not-found (the authz layer already handles this at decision
//     time; this management service gates on workspace write permission).
//   - 不复制资产: bindings reference knowledge_asset_versions.id, they never
//     copy content. Multiple agents pinning the same version share one row.
package binding

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// Sentinel errors. The handler maps these to the API error envelope:
//   - ErrBindingNotFound      → 404 + 40400 (no existence leak)
//   - ErrBindingConflict      → 409 + 40900 (ETag mismatch / concurrent update)
//   - ErrIdempotencyConflict  → 409 + 40900 (Idempotency-Key reused for a
//     different payload, §5.2 / §11.1)
//   - ErrIdempotentRetry      → returned to the service by the sink on a
//     same-payload retry; the service re-fetches and returns the original.
//   - ErrPinnedVersionAlert  → a batch contained a pinned binding whose
//     pinned version is not usable (revoked/missing). The binding is still
//     written (so the alert is durable and auditable) but the caller is told
//     the binding is in a blocked state — it will阻断 use until the version
//     is restored or the binding is repinned (§5.1 阻断+告警, not fallback).
var (
	ErrBindingNotFound     = errors.New("binding: not found")
	ErrBindingConflict     = errors.New("binding: etag conflict")
	ErrIdempotencyConflict = errors.New("binding: idempotency-key conflict")
	ErrIdempotentRetry     = errors.New("binding: idempotent retry")
	ErrPinnedVersionAlert  = errors.New("binding: pinned version not usable (blocked, no fallback)")
)

// ErrInvalidBinding is returned for a structurally invalid binding input
// (scope/policy mismatch) — a 400. The DB CHECK constraints also reject these,
// but validating up front lets the service return a precise error per item
// in a batch instead of a single opaque constraint violation.
var ErrInvalidBinding = errors.New("binding: invalid")

// BindingInput is one item in a batch upsert. ID empty → create; ID set →
// update (gated by ETag). VersionPolicy=pinned requires ScopeKind=asset AND a
// non-nil PinnedVersionID (Phase 0 CHECK constraints mirror this; the service
// validates up front so a batch reports the offending item).
type BindingInput struct {
	ID              *uuid.UUID                  // nil on create, set on update
	ETag            int64                       // required when ID set (If-Match)
	ScopeKind       domain.BindingScopeKind     // asset|workspace|asset_type
	AssetID         *uuid.UUID                  // required when ScopeKind=asset
	AssetType       *domain.AssetType           // required when ScopeKind=asset_type
	Effect          domain.BindingEffect        // allow|deny
	VersionPolicy   domain.BindingVersionPolicy // follow_published|pinned
	PinnedVersionID *uuid.UUID                  // required when VersionPolicy=pinned
	DeliveryMode    domain.BindingDeliveryMode  // tool|summary|inline
	Priority        int
}

// BindingResult is the outcome for one batch item. A pinned binding whose
// version is not usable is written (durable alert) and flagged
// PinnedVersionBlocked=true; the per-batch BatchResult.Alerted collects these
// so the caller surfaces §5.1's "阻断+告警" (block at decision time + alert now).
type BindingResult struct {
	Binding              domain.AgentBinding
	PinnedVersionBlocked bool // pinned version not usable → use will阻断, no fallback
}

// BatchResult aggregates a batch outcome. Results are 1:1 with the input
// items, preserving order. Alerted lists the indexes of items whose pinned
// version is blocked (the caller should surface these as alerts). NewRevision
// is the workspace authz revision after the batch (bumped in the same tx).
type BatchResult struct {
	Results       []BindingResult
	Alerted       []int
	NewRevision   int64
	IdempotentHit bool // true when the batch was an idempotent retry (original returned)
}

// BatchUpsert applies a batch of create/update binding operations for ONE agent
// in ONE transaction (§5.2). It writes agent.binding_changed outbox events
// (one per changed binding) and bumps workspace_authz_revisions.revision in
// the same tx — so the next authz request sees the new effective set (§5.4
// cache invalidation by revision). Idempotency-Key is enforced: a duplicate
// key for a DIFFERENT payload returns ErrIdempotencyConflict; the same payload
// returns the original batch (ErrIdempotentRetry, resolved by the service).
type BatchUpsert interface {
	BatchUpsert(ctx context.Context, agentID, workspaceID uuid.UUID, idempotencyKey string, inputs []BindingInput, actor domain.EventActor) (BatchResult, error)
}

// RevokeRevoker revokes a single binding (sets revoked_at=now()) AND bumps the
// workspace authz revision in the same transaction (§5.4: revoke → revision+1
// → cache invalidates → next request denies). It also writes an
// agent.binding_changed outbox event. Same-tx semantics mirror
// delegated_sessions.Revoke (the linearization point).
type RevokeRevoker interface {
	Revoke(ctx context.Context, bindingID, agentID, workspaceID uuid.UUID, actor domain.EventActor) (int64, error)
}

// Repository is the CRUD port over agent_bindings (read paths + single-row
// writes the service needs outside the batch sink). The batch sink owns the
// transactional double-write; this port covers list/get/etag lookup.
type Repository interface {
	// List returns the active (revoked_at IS NULL) bindings for an agent,
	// cursor-paginated (§6.1 GET /agents/{id}/bindings).
	List(ctx context.Context, agentID, workspaceID uuid.UUID, after *uuid.UUID, limit int) ([]domain.AgentBinding, error)
	// Get returns a single binding by id (active or revoked).
	Get(ctx context.Context, id uuid.UUID) (domain.AgentBinding, error)
	// GetByIdempotencyKey loads a batch's bindings by the idempotency_key stored
	// on the batch. Used to satisfy an idempotent retry (§5.2).
	GetByIdempotencyKey(ctx context.Context, key string) ([]domain.AgentBinding, error)
	// ActiveForAgent returns ALL active (revoked_at IS NULL) bindings for an
	// agent in a workspace — unpaginated, for the §6.2 delivery path's
	// effective-binding resolution (§5.3 precedence: the delivery service picks
	// the winner by deny>allow, priority, scope-narrowness). A binding set is
	// small (tens), so loading the full set in one query is cheaper than a
	// per-scope fan-out. The caller MUST hold `assign` on the workspace (the
	// delivery service gates this; this port does not re-check).
	ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error)
}

// PinnedVersionChecker reports whether a pinned version is usable
// (build_status='ready' AND governance_status='published'). The authz layer's
// AssetVersionRepo already exposes this shape; this port lets the binding
// service detect blocked pinned bindings at batch time so it can ALERT
// (§5.1: 阻断+告警). A nil checker disables alerting (acceptable in tests).
type PinnedVersionChecker interface {
	IsUsable(ctx context.Context, versionID uuid.UUID) (bool, error)
}
