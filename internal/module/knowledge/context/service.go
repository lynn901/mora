// Package context — assembly seam (design-docs/19 §3.3).
//
// This file is the wiring layer between the Broker (broker.go) and the
// production dependencies: it builds the default PolicyLoader over a
// context_authority_policies repo (§5.3 — DB row when present, built-in
// default when ErrPolicyNotFound), the PrincipalResolver from an HTTP AuthState,
// and the Broker itself from the typed ports + authz service. The handler
// (handler.go) calls these; cmd/mora-api wires the real ports + repo.
//
// The assembly lives in the context package (not a separate wiring package)
// because the Broker's constructor + the PolicyLoader are pure functions over
// the package's own ports — no infra import here. The infra/postgres
// ContextPolicyRepo satisfies PolicyRepo (YS-202); the real authz.Service
// satisfies AuthzService; the real adapters (adapters.go) satisfy the typed
// ports. This file only composes them.
package context

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// DefaultPolicyLoader returns a PolicyLoader over a PolicyRepo (§5.3). It loads
// the is_current row for (workspace, intent); on ErrPolicyNotFound it falls
// back to the built-in default (NewAuthorityPolicy with a zero overlay) and
// reports policy_version=0 (the "default, not DB" sentinel). Any other repo
// error propagates (the Broker fails closed — no query serves a broken
// policy repo, §5.3).
//
// The loaded policy's Intent() MUST match the routed intent; NewAuthorityPolicy
// guarantees this for the four known intents. An unknown intent yields nil →
// the Broker returns ErrBrokerNoPolicy (a fatal wiring fault, surfaced not
// silently fallen-back).
func DefaultPolicyLoader(repo PolicyRepo) PolicyLoader {
	return func(ctx context.Context, workspaceID uuid.UUID, intent Intent) (AuthorityPolicy, int, error) {
		rec, err := repo.LoadCurrent(ctx, workspaceID, intent)
		if err != nil {
			if errors.Is(err, ErrPolicyNotFound) {
				// §5.3: fall back to the built-in default. policy_version=0
				// signals "default, not DB-sourced" to the audit summary.
				p := NewAuthorityPolicy(intent, PolicyConfig{})
				if p == nil {
					return nil, 0, ErrBrokerNoPolicy
				}
				return p, 0, nil
			}
			return nil, 0, err
		}
		p := NewAuthorityPolicy(intent, rec.Config)
		if p == nil {
			return nil, 0, ErrBrokerNoPolicy
		}
		return p, rec.PolicyVersion, nil
	}
}

// ValidatePolicyConfig enforces the §7.2 mutual-exclusion constraint the PUT
// handler checks before storing a policy: exclude_when ∩ must_surface_conflicts
// MUST be empty (§7.2 "同一标签不得同时出现在一个策略的 exclude_when 与
// must_surface_conflicts"). A violation is rejected with a 400 at the handler;
// this helper returns true when the config is valid, false + the colliding
// tags when not. It also enforces the §2.1 schema: intent is one of the four
// known values, weights are in [0,1], primary_basis types are known asset
// types. The handler calls this BEFORE Upsert so an invalid config never
// reaches the repo (§5.3 — the repo is the only writer; the gate is here).
//
// The six-field schema (§2.1 / §2.2): primary_basis, must_surface_conflicts,
// weights, exclude_when (+ the versioned is_current/policy_version the repo
// owns). This helper validates the four config fields; the versioned fields
// are the repo's concern.
func ValidatePolicyConfig(cfg PolicyConfig) (collisions []string, ok bool) {
	// §7.2 mutual-exclusion: exclude_when ∩ must_surface_conflicts = ∅.
	if len(cfg.ExcludeWhen) > 0 && len(cfg.MustSurfaceConflicts) > 0 {
		surface := make(map[string]bool, len(cfg.MustSurfaceConflicts))
		for _, t := range cfg.MustSurfaceConflicts {
			surface[t] = true
		}
		for _, t := range cfg.ExcludeWhen {
			if surface[t] {
				collisions = append(collisions, t)
			}
		}
		if len(collisions) > 0 {
			return collisions, false
		}
	}
	// weights in [0,1] (§5.1).
	for t, w := range cfg.Weights {
		if !knownAssetType(t) {
			return nil, false // unknown type in weights → reject
		}
		if w < 0 || w > 1 {
			return nil, false
		}
	}
	// primary_basis types must be known (§5.1).
	for _, t := range cfg.PrimaryBasis {
		if !knownAssetType(t) {
			return nil, false
		}
	}
	return nil, true
}

// knownAssetType reports whether t is one of the four asset types the Broker
// orchestrates (§1.2 013 knowledge_core).
func knownAssetType(t domain.AssetType) bool {
	switch t {
	case domain.AssetTypeDocument, domain.AssetTypeCodebase, domain.AssetTypeMemory, domain.AssetTypeSkill:
		return true
	}
	return false
}

// KnownIntents is the four built-in intents (§4.1), in canonical order. The
// GET /knowledge/policies handler iterates this to list the policies for an
// intent even when the repo has no row (it reports the built-in default,
// policy_version=0, §5.3).
var KnownIntents = []Intent{IntentSpec, IntentRevision, IntentRationale, IntentProcedure}

// PolicyResponse is the §11.1 GET /knowledge/policies list element + the
// PUT /knowledge/policies/{intent} response. Carries the versioned config +
// the is_current flag. Field names align with the §2.1 table.
type PolicyResponse struct {
	Intent               Intent                `json:"intent"`
	PolicyVersion        int                   `json:"policy_version"`
	IsCurrent            bool                  `json:"is_current"`
	PrimaryBasis         []domain.AssetType   `json:"primary_basis"`
	MustSurfaceConflicts []string              `json:"must_surface_conflicts"`
	Weights              map[domain.AssetType]float64 `json:"weights"`
	ExcludeWhen          []string              `json:"exclude_when"`
}

// BuiltinPolicyResponse is the §5.3 fallback response for an intent with no DB
// row: it reports the built-in default config at policy_version=0 (the
// "default, not DB-sourced" sentinel). The handler calls this when
// PolicyRepo.LoadCurrent returns ErrPolicyNotFound so the GET /knowledge/
// policies list always covers all four intents (§5.3).
func BuiltinPolicyResponse(intent Intent) PolicyResponse {
	p := NewAuthorityPolicy(intent, PolicyConfig{})
	if p == nil {
		return PolicyResponse{Intent: intent, PolicyVersion: 0, IsCurrent: true}
	}
	cfg := policyConfig(p)
	return PolicyResponse{
		Intent:               intent,
		PolicyVersion:        0,
		IsCurrent:            true,
		PrimaryBasis:         cfg.PrimaryBasis,
		MustSurfaceConflicts: cfg.MustSurfaceConflicts,
		Weights:              cfg.Weights,
		ExcludeWhen:          cfg.ExcludeWhen,
	}
}

// policyConfig extracts the PolicyConfig from a built-in AuthorityPolicy. The
// builtInPolicy carries it on cfg; the port does not expose it, so we
// type-assert. An unknown policy implementation yields the zero config (the
// handler reports an empty config rather than crashing the GET).
func policyConfig(p AuthorityPolicy) PolicyConfig {
	if bp, ok := p.(*builtInPolicy); ok {
		return bp.cfg
	}
	return PolicyConfig{}
}
