// Package context — authority policy domain + port (design-docs/19 §5).
//
// The AuthorityPolicy port + four built-in policies + the versioned
// context_authority_policies repo port live here. This file is the domain +
// port layer; the policy scoring logic (§5.2 Score / ConflictsToSurface) and
// the Intent Router (§4) are Stage 2 (YS-203). Stage 1 ships the port, the
// domain types, and the PG repo so Stage 2 can load + score against a real
// versioned config.
package context

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// ErrPolicyNotFound is returned by PolicyRepo.LoadCurrent when no is_current
// policy exists for (workspace, intent) — the caller falls back to the built-in
// defaults (§5.3). Existence-safe: a missing policy is not distinguishable
// from a denied read for an unauthorized caller at this layer (the Broker
// gates on authz before calling the repo).
var ErrPolicyNotFound = errors.New("context: authority policy not found")

// Intent is the query intent that selects the authority policy (doc 12 §9.5,
// design-docs/19 §4.1). Four built-in intents map 1:1 to the four built-in
// authority policies. The intent value is the storage key for
// context_authority_policies.intent.
type Intent string

const (
	IntentSpec      Intent = "spec"      // 规范要求：当前有效且经治理批准的文档
	IntentRevision  Intent = "revision"  // revision 实现：固定 commit 的代码/配置/迁移/测试
	IntentRationale Intent = "rationale" // 决策原因：决策文档、审核 Memory 与证据
	IntentProcedure Intent = "procedure" // 执行流程：已批准 Skill、Runbook、环境约束
)

// PolicyConfig is the JSONB config shape on context_authority_policies (§2.1).
// The four built-in policies' defaults are provided by the architecture (§5.1
// table); the PM governs the actual stored values. This struct is the schema
// the repo (de)serializes; unknown keys survive in Raw for forward-compat.
type PolicyConfig struct {
	// PrimaryBasis is the asset_type list that is the primary basis under this
	// intent (§5.1 first column).
	PrimaryBasis []domain.AssetType `json:"primary_basis"`
	// MustSurfaceConflicts are the conflict relation types this policy must
	// keep side-by-side even if low-scoring (§5.1 / §7.2).
	MustSurfaceConflicts []string `json:"must_surface_conflicts"`
	// Weights are the per-asset_type authority weights in [0,1] (§5.1).
	Weights map[domain.AssetType]float64 `json:"weights"`
	// ExcludeWhen are the conditions that drop a candidate by default
	// (deprecated / version_mismatch, §7.2).
	ExcludeWhen []string `json:"exclude_when"`
	// Raw preserves unknown keys so a newer config survives a round-trip
	// through an older binary (forward-compat; not surfaced to the agent).
	Raw map[string]any `json:"-"`
}

// AuthorityPolicyRecord is one versioned policy row (§2.1). The repo returns
// the is_current row for a (workspace, intent); version increments on update.
type AuthorityPolicyRecord struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	Intent         Intent
	PolicyVersion  int
	IsCurrent      bool
	Config         PolicyConfig
	CreatedAt      time.Time
	SupersededAt   *time.Time
	CreatedByID    *uuid.UUID
}

// PolicyRepo is the persistence port over context_authority_policies (§5.3).
// Load: read the is_current row for a (workspace, intent). Update: supersede
// the current row (is_current=false, superseded_at=now) and insert a new
// policy_version+1 is_current row — versioned, audit-safe (§0 D5). The repo
// is the only writer; the Broker only reads.
type PolicyRepo interface {
	// LoadCurrent returns the is_current policy for (workspace, intent). A
	// missing row yields ErrPolicyNotFound (the caller — Broker or a config
	// bootstrap — falls back to the built-in defaults, §5.3).
	LoadCurrent(ctx context.Context, workspaceID uuid.UUID, intent Intent) (AuthorityPolicyRecord, error)
	// Upsert creates policy_version+1, superseding the prior current (§5.3).
	// If no prior row exists, policy_version=1. The is_current exclusion is
	// enforced by the table constraint (migration 024).
	Upsert(ctx context.Context, rec AuthorityPolicyRecord) (AuthorityPolicyRecord, error)
	// ListByWorkspace returns the is_current policies for all intents in a
	// workspace (used by GET /knowledge/policies, §11.1).
	ListByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]AuthorityPolicyRecord, error)
	// CurrentVersion returns the highest policy_version for (workspace,
	// intent); 0 when none. Used as a cache key (§0 D10 / §5.3).
	CurrentVersion(ctx context.Context, workspaceID uuid.UUID, intent Intent) (int, error)
}
