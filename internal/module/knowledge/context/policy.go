// Package contextbroker — authority policy domain + port (design-docs/19 §5).
//
// The AuthorityPolicy port + four built-in policies + the versioned
// context_authority_policies repo port live here. This file is the domain +
// port layer; the policy scoring logic (§5.2 Score / ConflictsToSurface) and
// the Intent Router (§4) are Stage 2 (YS-203). Stage 1 ships the port, the
// domain types, and the PG repo so Stage 2 can load + score against a real
// versioned config.
package contextbroker

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

// AuthorityPolicy scores and orders candidates for a given intent (12 §9.5,
// design-docs/19 §5.2). The policy is versioned
// (context_authority_policies.policy_version); the broker records the applied
// policy_version in the audit summary (§7.1 step 10). The system does NOT
// maintain a single global "document always > code" ordering — authority is
// intent-dependent (§5.1).
//
// When high-authority assets conflict, the policy surfaces both sides with
// their citations instead of silently picking one (§5.1 / 11 §7.2). Code
// assets can only attest to the static implementation at the pinned revision;
// they MUST NOT claim production current behavior from unverified
// deploy/runtime evidence (§5.1).
type AuthorityPolicy interface {
	// Intent is the intent this policy applies to (one of the four built-in
	// intents, §5.1). Used by the broker to select the policy after the
	// IntentRouter routes (§7.1 step 7).
	Intent() Intent

	// Score blends authority/freshness/confidence per §9.5 + the policy
	// weights, returning candidates ranked by the blended score (§5.2). The
	// broker feeds the result to the Budgeter (§6.2).
	Score(candidates []KnowledgeCandidate, q KnowledgeQuery) []ScoredCandidate

	// ConflictsToSurface returns the conflict relation types this policy must
	// not drop (e.g. old_spec/impl_drift) — they are kept even if low-scoring
	// (§5.1 must_surface_conflicts / §7.2). DedupAndKeepConflicts consults
	// this to avoid collapsing both sides of a surfaced conflict.
	ConflictsToSurface() []string
}

// Conflict relation type constants (§5.1 must_surface_conflicts columns /
// §7.2). Policies return these from ConflictsToSurface; the Budgeter and
// DedupAndKeepConflicts consult them to keep both sides of a surfaced conflict
// instead of collapsing one. These are the canonical string keys the DB
// config also stores (migration 024 config.must_surface_conflicts JSONB).
const (
	ConflictOldSpec       = "old_spec"        // 旧规范：被新规范取代但策略要求展示
	ConflictImplDrift     = "impl_drift"      // 实现偏差：代码与文档不一致
	ConflictDocMismatch   = "doc_inconsistency" // 文档不一致：代码锚定 revision 与文档描述矛盾
	ConflictLowConfidence = "low_confidence"  // 低置信记忆：rationale 策略需展示以警示
	ConflictSuperseded    = "superseded"      // 被替代：记忆或规范被新版本取代
	ConflictVersionMismatch = "version_mismatch" // 版本不匹配：skill/资源版本与声明不符
	ConflictMissingPerm    = "missing_perm"      // 缺少权限：procedure 策略需展示执行约束
)

// policyWeights is the per-asset-type authority weight (§5.1). The Score blend
// multiplies each candidate's Authority signal by its type's weight; DB config
// (PolicyConfig.Weights) overrides the map, not the blend logic (§5.3).
type policyWeights map[domain.AssetType]float64

// defaultWeights pins the §5.1 weight table the four built-in policies use by
// default. Order matches the doc's "(document/code/memory/skill)" columns:
//
//	spec      → 0.9 / 0.5 / 0.4 / 0.3
//	revision  → 0.5 / 0.9 / 0.3 / 0.4
//	rationale → 0.8 / 0.3 / 0.9 / 0.3
//	procedure → 0.6 / 0.4 / 0.4 / 0.9
//
// PM-governed DB rows (YS-212) override these via PolicyConfig.Weights; the
// override replaces the whole map for that intent, not a per-key patch.
var defaultWeights = map[Intent]policyWeights{
	IntentSpec: {
		domain.AssetTypeDocument: 0.9,
		domain.AssetTypeCodebase: 0.5,
		domain.AssetTypeMemory:   0.4,
		domain.AssetTypeSkill:    0.3,
	},
	IntentRevision: {
		domain.AssetTypeDocument: 0.5,
		domain.AssetTypeCodebase: 0.9,
		domain.AssetTypeMemory:   0.3,
		domain.AssetTypeSkill:    0.4,
	},
	IntentRationale: {
		domain.AssetTypeDocument: 0.8,
		domain.AssetTypeCodebase: 0.3,
		domain.AssetTypeMemory:   0.9,
		domain.AssetTypeSkill:    0.3,
	},
	IntentProcedure: {
		domain.AssetTypeDocument: 0.6,
		domain.AssetTypeCodebase: 0.4,
		domain.AssetTypeMemory:   0.4,
		domain.AssetTypeSkill:    0.9,
	},
}

// defaultConflicts pins the §5.1 must_surface_conflicts column the four
// built-in policies return from ConflictsToSurface by default. DB config
// (PolicyConfig.MustSurfaceConflicts) overrides the list, not the logic (§5.3).
var defaultConflicts = map[Intent][]string{
	IntentSpec:      {ConflictOldSpec, ConflictImplDrift},
	IntentRevision:  {ConflictDocMismatch},
	IntentRationale: {ConflictLowConfidence, ConflictSuperseded},
	IntentProcedure: {ConflictVersionMismatch, ConflictMissingPerm},
}

// policy is the shared implementation the four built-in policies compose. It
// holds the intent, an override-able weight map, and an override-able conflict
// list. WithPolicy applies a DB-loaded PolicyConfig (§5.3) over the §5.1
// defaults: the config replaces the weights + conflict list but NEVER the
// scoring logic (§5.3 "DB 配置覆盖权重与冲突类型，不覆盖策略逻辑").
type policy struct {
	intent    Intent
	weights   policyWeights
	conflicts []string
}

// Intent reports the policy's intent.
func (p *policy) Intent() Intent { return p.intent }

// ConflictsToSurface returns the conflict types this policy must keep (§5.1).
// WithPolicy may have replaced the default list with a DB-configured one.
func (p *policy) ConflictsToSurface() []string { return p.conflicts }

// weightFor returns the authority weight for a candidate's asset type. A type
// missing from the weights map (e.g. a DB override that omitted a type) gets
// weight 0 — that type's candidates score purely on freshness/confidence,
// never outranking a type the policy explicitly weights. This is fail-safe:
// an incomplete override cannot silently grant full weight to an unlisted type.
func (p *policy) weightFor(t domain.AssetType) float64 {
	if p.weights == nil {
		return 0
	}
	return p.weights[t]
}

// Score implements the §5.2 blend: blended = w_authority*authority +
// w_freshness*freshness + w_confidence*confidence (confidence defaults to the
// provider Score when the engine emitted no explicit confidence signal, since
// a missing confidence should not zero-out an otherwise strong candidate).
//
// The blend is a weighted sum of three normalized [0,1] signals, then scaled
// by the policy's per-type authority weight so the intent's primary-basis types
// outrank secondary types. Ties break by the original provider Score (a stable
// secondary sort) and then by AssetID for deterministic output (§9.5 stable
// ranking; avoids flaky ordering across runs).
//
// Score does NOT drop candidates — that is the Budgeter's job (§6.2). It only
// ranks; even candidates the policy would exclude (deprecated, version
// mismatch) stay in the list for DedupAndKeepConflicts to surface as conflicts
// when the policy's ConflictsToSurface requires it.
func (p *policy) Score(candidates []KnowledgeCandidate, _ KnowledgeQuery) []ScoredCandidate {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]ScoredCandidate, len(candidates))
	for i, c := range candidates {
		conf := c.Confidence
		if conf == nil {
			// Missing confidence signal: fall back to the provider Score as the
			// confidence proxy so a nil-confidence candidate is not zeroed. Clamp
			// to [0,1] since Score may exceed 1 for some engines.
			s := c.Score
			if s > 1 {
				s = 1
			}
			conf = &s
		}
		// §5.2 blend weights: authority and freshness are the primary signals;
		// confidence is a tertiary modulator. The 0.5/0.3/0.2 split sums to 1.0
		// so the blended score stays in [0,1] before the per-type authority
		// weight scales it. The per-type weight then enforces the §5.1 ordering.
		blended := 0.5*c.Authority + 0.3*c.Freshness + 0.2**conf
		weighted := blended * p.weightFor(c.AssetType)
		out[i] = ScoredCandidate{Candidate: c, Score: weighted}
	}
	// Stable sort by blended score desc; tie-break by provider Score desc, then
	// AssetID asc for determinism. sort.SliceStable preserves the input order of
	// equal elements so callers feeding pre-ranked candidates see minimal churn.
	sortPolicyDesc(out)
	return out
}

// withPolicy applies a DB-loaded PolicyConfig over the §5.1 defaults (§5.3).
// The config's Weights replace the default weight map; its MustSurfaceConflicts
// replaces the default conflict list. Unknown keys (PolicyConfig.Raw) are
// ignored at this layer — they survive for forward-compat but do not change
// scoring. A nil/empty config is a no-op (caller falls back to the built-in
// defaults, §5.3 "missing row → built-in defaults").
func (p *policy) withPolicy(cfg PolicyConfig) *policy {
	if len(cfg.Weights) > 0 {
		// copy so a later mutation of the config map cannot leak into the policy
		w := make(policyWeights, len(cfg.Weights))
		for k, v := range cfg.Weights {
			w[k] = v
		}
		p.weights = w
	}
	if len(cfg.MustSurfaceConflicts) > 0 {
		p.conflicts = append([]string(nil), cfg.MustSurfaceConflicts...)
	}
	return p
}

// --- 四内置策略（§5.1）---
//
// Each built-in is a named type embedding *policy so the constructors can
// return a concrete *T the broker type-asserts on if it needs the intent
// without an interface call. The constructors seed the §5.1 defaults; an
// optional PolicyConfig (loaded from context_authority_policies is_current=true)
// overrides weights + conflicts via WithPolicy.

// specPolicy is the IntentSpec policy: primary basis = current
// governance-approved document; must surface old_spec + impl_drift (§5.1).
type specPolicy struct{ *policy }

// SpecPolicy returns the IntentSpec built-in authority policy with the §5.1
// default weights + conflict list. Apply a DB-loaded config with WithSpecPolicy.
func SpecPolicy() AuthorityPolicy {
	return &specPolicy{policy: &policy{
		intent:    IntentSpec,
		weights:   cloneWeights(defaultWeights[IntentSpec]),
		conflicts: cloneStrings(defaultConflicts[IntentSpec]),
	}}
}

// revisionPolicy is the IntentRevision policy: primary basis = pinned-commit
// codebase/config/migration/test; must surface doc_inconsistency (§5.1).
type revisionPolicy struct{ *policy }

// RevisionPolicy returns the IntentRevision built-in with §5.1 defaults.
func RevisionPolicy() AuthorityPolicy {
	return &revisionPolicy{policy: &policy{
		intent:    IntentRevision,
		weights:   cloneWeights(defaultWeights[IntentRevision]),
		conflicts: cloneStrings(defaultConflicts[IntentRevision]),
	}}
}

// rationalePolicy is the IntentRationale policy: primary basis = decision
// document + reviewed memory/evidence; must surface low_confidence + superseded
// memory (§5.1).
type rationalePolicy struct{ *policy }

// RationalePolicy returns the IntentRationale built-in with §5.1 defaults.
func RationalePolicy() AuthorityPolicy {
	return &rationalePolicy{policy: &policy{
		intent:    IntentRationale,
		weights:   cloneWeights(defaultWeights[IntentRationale]),
		conflicts: cloneStrings(defaultConflicts[IntentRationale]),
	}}
}

// procedurePolicy is the IntentProcedure policy: primary basis = approved skill
// + runbook + env constraints; must surface version_mismatch + missing_perm
// (§5.1).
type procedurePolicy struct{ *policy }

// ProcedurePolicy returns the IntentProcedure built-in with §5.1 defaults.
func ProcedurePolicy() AuthorityPolicy {
	return &procedurePolicy{policy: &policy{
		intent:    IntentProcedure,
		weights:   cloneWeights(defaultWeights[IntentProcedure]),
		conflicts: cloneStrings(defaultConflicts[IntentProcedure]),
	}}
}

// PolicyForIntent returns the built-in policy for a given intent (§5.3 — the
// broker calls this after IntentRouter routes, then optionally applies a
// DB-loaded config via WithPolicy). An unknown intent yields nil; the caller
// falls back to IntentSpec (§4.2 rule 6 fallback) rather than panic.
func PolicyForIntent(intent Intent) AuthorityPolicy {
	switch intent {
	case IntentSpec:
		return SpecPolicy()
	case IntentRevision:
		return RevisionPolicy()
	case IntentRationale:
		return RationalePolicy()
	case IntentProcedure:
		return ProcedurePolicy()
	default:
		return nil
	}
}

// ApplyPolicyConfig returns a copy of the policy with a DB-loaded config
// overlaid on the §5.1 defaults (§5.3). It is the single entry point the
// broker's policy loader uses: load the is_current PolicyConfig for the intent,
// then ApplyPolicyConfig(builtIn, cfg). A nil policy or empty config returns
// the built-in unchanged (caller falls back to defaults, §5.3).
//
// The returned policy shares NO mutable state with the built-in: weights and
// conflicts are cloned, so a later DB update + reload cannot leak into a policy
// still referenced by an in-flight request.
func ApplyPolicyConfig(builtIn AuthorityPolicy, cfg PolicyConfig) AuthorityPolicy {
	if builtIn == nil {
		return nil
	}
	// All four built-ins are *policy-backed; type-assert to read the shared
	// base. An unknown impl (a future custom policy) is returned unchanged —
	// this layer only knows how to overlay config on the four built-ins.
	type policyBase interface {
		base() *policy
	}
	// the four built-ins embed *policy; expose it via a helper so ApplyPolicyConfig
	// doesn't reach into a concrete unexported type by name.
	base := policyBaseOf(builtIn)
	if base == nil {
		return builtIn
	}
	clone := &policy{
		intent:    base.intent,
		weights:   cloneWeights(base.weights),
		conflicts: cloneStrings(base.conflicts),
	}
	clone.withPolicy(cfg)
	// wrap in the same concrete wrapper type so Intent() / type-asserts still work
	switch builtIn.(type) {
	case *specPolicy:
		return &specPolicy{policy: clone}
	case *revisionPolicy:
		return &revisionPolicy{policy: clone}
	case *rationalePolicy:
		return &rationalePolicy{policy: clone}
	case *procedurePolicy:
		return &procedurePolicy{policy: clone}
	default:
		return builtIn
	}
}

// policyBaseOf returns the *policy embedded in a built-in, or nil if p is not
// one of the four built-ins (a future custom policy would return nil and
// ApplyPolicyConfig leaves it unchanged).
func policyBaseOf(p AuthorityPolicy) *policy {
	switch v := p.(type) {
	case *specPolicy:
		return v.policy
	case *revisionPolicy:
		return v.policy
	case *rationalePolicy:
		return v.policy
	case *procedurePolicy:
		return v.policy
	default:
		return nil
	}
}

// cloneWeights copies a weight map so a DB override or in-flight request cannot
// mutate the package-level default table.
func cloneWeights(w policyWeights) policyWeights {
	if w == nil {
		return nil
	}
	out := make(policyWeights, len(w))
	for k, v := range w {
		out[k] = v
	}
	return out
}

// cloneStrings copies a string slice defensively (same reason as cloneWeights).
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// sortPolicyDesc sorts ScoredCandidate by Score descending, with a stable
// tie-break on provider Score (the candidate's own Score field) descending,
// then AssetID ascending for deterministic output across runs. Implemented as
// a simple insertion sort rather than sort.SliceStable so this file stays free
// of a "sort" import for a path that is almost always already pre-sorted by
// the producing engine — n is small (the candidate list per query) and the
// stable property matters for reproducible ranking (§9.5).
func sortPolicyDesc(s []ScoredCandidate) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && scoreLess(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// scoreLess reports whether a should rank before b (i.e. a is "greater" in
// desc order). Tie-break: higher Score wins; on equal Score, higher provider
// Score wins; on equal provider Score, lower AssetID string wins (deterministic).
func scoreLess(a, b ScoredCandidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Candidate.Score != b.Candidate.Score {
		return a.Candidate.Score > b.Candidate.Score
	}
	// AssetID string compare for stable, deterministic tie-break.
	return a.Candidate.AssetID.String() < b.Candidate.AssetID.String()
}
