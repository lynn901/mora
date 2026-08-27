// Package context — authority policy domain + port + four built-in policies
// (design-docs/19 §5).
//
// The AuthorityPolicy port, the four built-in policies, the versioned
// context_authority_policies repo port, and the policy factory live here. The
// port scores + orders candidates for an intent and declares which conflict
// relations must surface (§5.2). The four built-in policies carry the §5.1
// default weights/must-surface conflicts; NewAuthorityPolicy overlays a DB
// PolicyConfig onto those defaults so the PM-governed config (YS-212) overrides
// weights and conflict-type lists WITHOUT touching the scoring logic (§5.3).
package context

import (
	"context"
	"errors"
	"sort"
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

// ---------------------------------------------------------------------------
// AuthorityPolicy port + four built-in policies (§5.1, §5.2)
// ---------------------------------------------------------------------------

// AuthorityPolicy scores and orders candidates for a given intent (doc 12 §9.5,
// design-docs/19 §5.2). The policy is versioned (context_authority_policies.
// policy_version); the Broker records the applied policy_version in the audit
// summary (§9.2 step 10). The system does NOT maintain a single global
// "document always beats code" ordering — the order changes with intent (§5.1).
//
// Scoring blends three signals the producing engine already emitted
// (Authority / Freshness / Confidence) with the policy's per-asset_type weight
// (§5.1). When high-authority candidates conflict, the Broker returns both with
// their citations instead of silently picking one (11 §7.2); conflicts the
// policy declares must-surface are kept side-by-side even if low-scoring (§7.2).
type AuthorityPolicy interface {
	// Intent is the intent this policy scores for (§4.1).
	Intent() Intent
	// Score blends authority/freshness/confidence with the policy weights
	// (§5.2). The returned slice is sorted by blended Score descending; ties
	// keep the input order (stable). Provider Score is preserved on the
	// candidate; the blended Score is carried on ScoredCandidate.Score.
	Score(candidates []KnowledgeCandidate, q KnowledgeQuery) []ScoredCandidate
	// ConflictsToSurface returns the conflict relation types this policy must
	// NOT drop (e.g. contradicts/old_spec/impl_drift) — they are kept even if
	// low-scoring, so the Broker surfaces them side by side (§5.1 / §7.2).
	ConflictsToSurface() []string
}

// Signal blend weights (§9.5 authority/freshness/confidence blend). These are
// the architecture-provided defaults; the policy's per-asset_type authority
// weight (PolicyConfig.Weights, §5.1) multiplies the blend, NOT these. YS-212
// governs the per-type weights via DB config; the blend constants are stable
// architecture defaults (changing them is a design-doc change, not a config).
const (
	weightAuthority  = 0.5
	weightFreshness  = 0.3
	weightConfidence = 0.2
)

// defaultPolicyConfigs is the four built-in policies' defaults (§5.1 table).
// PrimaryBasis is informational (it documents the intent's primary source; the
// Intent Router does the type selection). Weights are the per-asset_type
// authority weights the Score blend multiplies. MustSurfaceConflicts are the
// conflict relation types the policy must keep side-by-side. The PM (YS-212)
// overrides Weights + MustSurfaceConflicts via DB config; the Go defaults here
// are the architecture-provided baseline (§5.1 "默认值由架构提供，PM 治理").
var defaultPolicyConfigs = map[Intent]PolicyConfig{
	IntentSpec: {
		PrimaryBasis:        []domain.AssetType{domain.AssetTypeDocument},
		MustSurfaceConflicts: []string{"old_spec", "impl_drift"},
		Weights: map[domain.AssetType]float64{
			domain.AssetTypeDocument: 0.9,
			domain.AssetTypeCodebase: 0.5,
			domain.AssetTypeMemory:   0.4,
			domain.AssetTypeSkill:    0.3,
		},
		ExcludeWhen: []string{"deprecated", "version_mismatch"},
	},
	IntentRevision: {
		PrimaryBasis:        []domain.AssetType{domain.AssetTypeCodebase},
		MustSurfaceConflicts: []string{"contradicts", "impl_drift"},
		Weights: map[domain.AssetType]float64{
			domain.AssetTypeDocument: 0.5,
			domain.AssetTypeCodebase: 0.9,
			domain.AssetTypeMemory:   0.3,
			domain.AssetTypeSkill:    0.4,
		},
		ExcludeWhen: []string{"deprecated", "version_mismatch"},
	},
	IntentRationale: {
		PrimaryBasis:        []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
		MustSurfaceConflicts: []string{"contradicts", "old_spec"},
		Weights: map[domain.AssetType]float64{
			domain.AssetTypeDocument: 0.8,
			domain.AssetTypeCodebase: 0.3,
			domain.AssetTypeMemory:   0.9,
			domain.AssetTypeSkill:    0.3,
		},
		ExcludeWhen: []string{"deprecated", "version_mismatch"},
	},
	IntentProcedure: {
		PrimaryBasis:        []domain.AssetType{domain.AssetTypeSkill, domain.AssetTypeDocument},
		MustSurfaceConflicts: []string{"version_mismatch", "impl_drift"},
		Weights: map[domain.AssetType]float64{
			domain.AssetTypeDocument: 0.6,
			domain.AssetTypeCodebase: 0.4,
			domain.AssetTypeMemory:   0.4,
			domain.AssetTypeSkill:    0.9,
		},
		ExcludeWhen: []string{"deprecated", "version_mismatch"},
	},
}

// builtInPolicy is the Go implementation of one built-in authority policy
// (§5.1). Its config is the §5.1 defaults overlaid with a DB PolicyConfig
// (NewAuthorityPolicy): the DB overrides Weights + MustSurfaceConflicts +
// ExcludeWhen + PrimaryBasis, but NOT the scoring logic — that stays in Score
// (§5.3 "DB 配置覆盖权重与冲突类型，不覆盖策略逻辑").
type builtInPolicy struct {
	intent Intent
	cfg    PolicyConfig
}

// Intent returns the policy's intent.
func (p *builtInPolicy) Intent() Intent { return p.intent }

// ConflictsToSurface returns the must-surface conflict types (§5.1 / §7.2).
// These candidates are kept side-by-side by the Broker even if low-scoring —
// the policy never silently picks one answer when high-authority sources
// conflict (11 §7.2).
func (p *builtInPolicy) ConflictsToSurface() []string {
	return p.cfg.MustSurfaceConflicts
}

// Score blends authority/freshness/confidence with the policy's per-type
// weight (§5.2, §9.5). The blend is:
//
//	blended = typeWeight * (weightAuthority*Authority
//	                       + weightFreshness*Freshness
//	                       + weightConfidence*Confidence)
//
// Confidence defaults to 0 when the engine emitted none (nil). The returned
// slice is sorted by blended score descending; ties preserve input order
// (sort.SliceStable, so the provider's ranking survives among equal scores).
//
// Exclusion conditions (ExcludeWhen: deprecated/version_mismatch, §7.2) are
// applied by the Broker's dedup pass (§7.2 step 6) BEFORE scoring — this
// method scores every candidate it is given. Keeping exclusion out of Score
// means the policy's only output is the ordering signal, and the dedup step
// stays the single gate for "does this candidate enter the result" (§7.2).
func (p *builtInPolicy) Score(candidates []KnowledgeCandidate, _ KnowledgeQuery) []ScoredCandidate {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]ScoredCandidate, 0, len(candidates))
	for _, c := range candidates {
		blend := weightAuthority*c.Authority + weightFreshness*c.Freshness
		blend += weightConfidence * confidenceOrZero(c.Confidence)
		typeWeight := p.typeWeight(c.AssetType)
		out = append(out, ScoredCandidate{
			Candidate: c,
			Score:     typeWeight * blend,
		})
	}
	// Stable sort so equal-blended candidates keep the provider's ranking
	// (§5.2 — policy re-scores ON TOP of the provider score, not replacing it).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

// typeWeight resolves the per-asset_type authority weight, defaulting to 0 for
// an unknown type (§5.1 — an unweighted type contributes no authority; it is
// not silently promoted). The default config covers all four asset types.
func (p *builtInPolicy) typeWeight(t domain.AssetType) float64 {
	if w, ok := p.cfg.Weights[t]; ok {
		return w
	}
	return 0
}

// confidenceOrZero dereferences a nil-safe confidence pointer (§9.5 — the
// engine may emit no confidence; it then contributes 0 to the blend).
func confidenceOrZero(c *float64) float64 {
	if c == nil {
		return 0
	}
	return *c
}

// NewAuthorityPolicy returns the built-in policy for an intent, overlaid with a
// DB PolicyConfig (§5.3). The DB config overrides Weights, MustSurfaceConflicts,
// ExcludeWhen, and PrimaryBasis; the scoring logic (Score) is NOT overridable
// (§5.3). When cfg is the zero value (no DB row — ErrPolicyNotFound), the §5.1
// defaults are used as-is. Unknown intent → nil (the caller — the Broker —
// treats a nil policy as a fatal wiring error, never a silent fallback, so a
// typo in the intent enum surfaces at startup, not at query time).
func NewAuthorityPolicy(intent Intent, overlay PolicyConfig) AuthorityPolicy {
	base := defaultPolicyConfigs[intent] // copy
	// Overlay non-zero DB fields onto the defaults. A zero PolicyConfig (no DB
	// row) leaves the §5.1 defaults intact. Map/slice nil checks: an empty
	// overlay field does NOT clear the default — only a non-empty field
	// overrides (§5.3 "覆盖", not "replace-with-empty").
	if len(overlay.PrimaryBasis) > 0 {
		base.PrimaryBasis = overlay.PrimaryBasis
	}
	if len(overlay.MustSurfaceConflicts) > 0 {
		base.MustSurfaceConflicts = overlay.MustSurfaceConflicts
	}
	if len(overlay.Weights) > 0 {
		base.Weights = overlay.Weights
	}
	if len(overlay.ExcludeWhen) > 0 {
		base.ExcludeWhen = overlay.ExcludeWhen
	}
	return &builtInPolicy{intent: intent, cfg: base}
}

// policyFor is a convenience for tests/wiring: the §5.1 defaults with no DB
// overlay (the path the Broker takes when PolicyRepo returns ErrPolicyNotFound,
// §5.3). NewAuthorityPolicy(zero) does the same; this names that intent.
func policyFor(intent Intent) AuthorityPolicy {
	return NewAuthorityPolicy(intent, PolicyConfig{})
}
