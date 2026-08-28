// policy.go — AuthorityPolicy 端口（§5.2）+ 四内置策略签名（§5.1）。
//
// The policy scores and orders candidates for a given intent (12 §9.5). It is
// versioned (context_authority_policies.policy_version); the broker records the
// applied policy_version in the audit summary (§7.1 step 10). The system does
// NOT maintain a single global "document always > code" ordering (§5.1) —
// authority is intent-dependent.
//
// When high-authority assets conflict, the policy surfaces both sides with
// their citations instead of silently picking one (§5.1 / 11 §7.2). Code
// assets can only attest to the static implementation at the pinned revision;
// they MUST NOT claim production current behavior from unverified
// deploy/runtime evidence (§5.1).
//
// Implementations land in a follow-up sub-task; the port + four built-in
// signatures are fixed here.

package contextbroker

import (
	"github.com/lynn901/mora/internal/domain"
)

// AuthorityPolicy scores and orders candidates for a given intent (12 §9.5).
// The policy is versioned (context_authority_policies.policy_version); the
// broker records the applied policy_version in the audit summary (§7.1 step
// 10).
type AuthorityPolicy interface {
	// Intent is the intent this policy applies to (one of the four built-in
	// intents, §5.1). Used by the broker to select the policy after the
	// IntentRouter routes (§7.1 step 7).
	Intent() Intent

	// Score blends authority/freshness/confidence/task-match per §9.5 + the
	// policy weights, returning candidates ranked by the blended score. The
	// broker feeds the result to the Budgeter (§6.2).
	Score(candidates []KnowledgeCandidate, q KnowledgeQuery) []ScoredCandidate

	// ConflictsToSurface returns the conflict relation types this policy must
	// not drop (e.g. contradicts/old_spec/impl_drift) — they are kept even if
	// low-scoring (§5.1 must_surface_conflicts / §7.2). DedupAndKeepConflicts
	// consults this to avoid collapsing both sides of a surfaced conflict.
	ConflictsToSurface() []string
}

// PolicyVersion is the versioned configuration row loaded from
// context_authority_policies (is_current=true). The Go built-in policy provides
// the scoring logic; the DB row overrides the weights + the
// must_surface_conflicts list, NOT the logic (§5.3). policy_version is part of
// the cache key (§5.3 / 附录 A #21) and is recorded in the audit summary.
type PolicyVersion struct {
	Intent        Intent
	PolicyVersion int
	PrimaryBasis   []domain.AssetType // §5.1 primary_basis
	MustSurfaceConflicts []string     // §5.1 must_surface_conflicts
	Weights        map[domain.AssetType]float64 // §5.1 权重 0..1
	ExcludeWhen    []string           // §2.1 config.exclude_when
}

// --- 四内置策略签名（§5.1）---
//
// Default weights (document/code/memory/skill) and must_surface_conflicts per
// §5.1 table. These are the Go-side defaults; PM-governed DB rows override the
// weights + conflict lists, not the scoring logic (§5.3). Implementations land
// in a follow-up sub-task; the constants pin the §5.1 values the policies use.

// SpecPolicy applies to IntentSpec: primary basis = current governance-approved
// document; must surface old spec + impl drift.
func SpecPolicy() AuthorityPolicy { return (*specPolicy)(nil) }

// RevisionPolicy applies to IntentRevision: primary basis = pinned-commit
// codebase/config/migration/test; must surface document inconsistency.
func RevisionPolicy() AuthorityPolicy { return (*revisionPolicy)(nil) }

// RationalePolicy applies to IntentRationale: primary basis = decision document
// + reviewed memory/evidence; must surface low-confidence or superseded memory.
func RationalePolicy() AuthorityPolicy { return (*rationalePolicy)(nil) }

// ProcedurePolicy applies to IntentProcedure: primary basis = approved skill +
// runbook + env constraints; must surface version mismatch or missing perm.
func ProcedurePolicy() AuthorityPolicy { return (*procedurePolicy)(nil) }

// specPolicy, revisionPolicy, rationalePolicy, procedurePolicy are the four
// built-in policy implementations (§5.1). Declared as pointer-to-named-type so
// the constructors above return a typed nil the broker can type-assert on; the
// methods land in a follow-up sub-task (TODO: implement Score/ConflictsToSurface).
type (
	specPolicy     struct{ policy }
	revisionPolicy  struct{ policy }
	rationalePolicy struct{ policy }
	procedurePolicy struct{ policy }
)

// policy is the shared embedded struct the four built-in policies compose.
// Fields are populated from the PolicyVersion (DB-overridable weights + conflict
// lists) the loader hands the constructor (§5.3).
type policy struct {
	version PolicyVersion
}

// Intent reports the policy's intent (shared implementation; the four
// built-ins override only the version defaults). TODO: implement.
func (p *policy) Intent() Intent                       { return p.version.Intent }
func (p *policy) Score(_ []KnowledgeCandidate, _ KnowledgeQuery) []ScoredCandidate {
	return nil // TODO: §5.2 — blend authority/freshness/confidence/task-match
}
func (p *policy) ConflictsToSurface() []string { return p.version.MustSurfaceConflicts }
