// Package context — Intent Router (design-docs/19 §4, D4).
//
// First-version Intent Router uses RULE-BASED routing — no intent-classifier
// model (D4 / §10 open decision). It selects, from a KnowledgeQuery, the intent
// (which authority policy applies) and the target asset-type set (which typed
// ports to fan out to). It decides policy + types ONLY; it does NOT decide
// authorization (authz.Service computes AuthzContext independently, §4.2).
//
// The keyword table is a versioned-config concern (§4.2 "关键词表是版本化配置
// 的一部分，PM 可调，不在代码硬编码"). The first version hard-codes the §4.2
// table BUT the Router accepts a RoutingConfig so a PM-governed config (YS-212)
// can override the keyword sets without a code change. NewIntentRouter takes
// the config; a nil config falls back to DefaultRoutingConfig (the §4.2 table).
package context

import (
	"context"
	"strings"

	"github.com/lynn901/mora/internal/domain"
)

// IntentRouter selects the query intent and target asset-type set (doc 12 §9.2
// step 3, design-docs/19 §4.3). First version is rule-based; model-based intent
// classification is deferred (§10).
type IntentRouter interface {
	// Route resolves (Intent, AssetTypes) from the query. The returned AssetTypes
	// is the non-empty set of typed ports the Broker fans out to (§4.2 rule 1:
	// an explicit caller-declared set wins; otherwise the keyword match picks).
	// Route never returns an empty AssetTypes slice — the fallback is
	// [document, memory] (§4.2 rule 6).
	Route(ctx context.Context, q KnowledgeQuery) (Intent, []domain.AssetType, error)
}

// RoutingConfig is the versioned-config keyword table (§4.2). The first version
// hard-codes DefaultRoutingConfig; the PM (YS-212) overrides the keyword sets
// via DB config so the table is tunable without a code change. Each intent has
// a KeywordSet: a candidate matches when the query contains ANY keyword in the
// set (case-insensitive substring). KeywordSet is a slice (not a map) so the
// config round-trips through JSONB without key-collision surprises; order is
// not significant for matching.
type RoutingConfig struct {
	// Spec keywords → IntentSpec, types [document] (§4.2 rule 2).
	Spec KeywordSet
	// Revision keywords → IntentRevision, types [codebase, document]
	// (§4.2 rule 3).
	Revision KeywordSet
	// Rationale keywords → IntentRationale, types [document, memory]
	// (§4.2 rule 4).
	Rationale KeywordSet
	// Procedure keywords → IntentProcedure, types [skill, document]
	// (§4.2 rule 5).
	Procedure KeywordSet
}

// KeywordSet is the keyword list for one intent (§4.2). A query matches when it
// contains any keyword (case-insensitive substring). Kept exported so the
// config repo (YS-212) can (de)serialize it.
type KeywordSet []string

// DefaultRoutingConfig is the §4.2 keyword table — the architecture-provided
// first version. The PM (YS-212) overrides this via DB config; until then this
// is what the Router uses. Bilingual (zh + en) because the product serves both.
var DefaultRoutingConfig = RoutingConfig{
	Spec: KeywordSet{
		"规范", "要求", "规格", "should", "must",
	},
	Revision: KeywordSet{
		"实现", "代码", "函数", "调用", "commit", "revision",
	},
	Rationale: KeywordSet{
		"为什么", "决策", "原因", "why", "rationale",
	},
	Procedure: KeywordSet{
		"如何执行", "流程", "步骤", "runbook", "how",
	},
}

// router is the rule-based IntentRouter (§4.2).
type router struct {
	cfg RoutingConfig
}

// NewIntentRouter builds a rule-based IntentRouter. A nil cfg falls back to
// DefaultRoutingConfig (the §4.2 table) so the wiring layer can pass nil when
// no PM config is loaded yet. The config is the injection point YS-212 fills;
// the scoring logic (§4.2 six rules) is NOT config-overridable — only the
// keyword sets are (§4.2 "关键词表是版本化配置的一部分").
func NewIntentRouter(cfg *RoutingConfig) IntentRouter {
	if cfg == nil {
		c := DefaultRoutingConfig // copy so the caller cannot mutate the default
		return &router{cfg: c}
	}
	return &router{cfg: *cfg}
}

// Route implements §4.2's six routing rules in order:
//
//  1. Explicit AssetTypes non-empty → use the caller's declared set; Intent is
//     inferred from the query keywords (rule 1). The explicit set wins because
//     the caller knows which ports they want; the keyword still picks the
//     POLICY (authority order changes with intent even when types are pinned).
//  2. query matches Spec keywords → IntentSpec, [document]
//  3. query matches Revision keywords → IntentRevision, [codebase, document]
//  4. query matches Rationale keywords → IntentRationale, [document, memory]
//  5. query matches Procedure keywords → IntentProcedure, [skill, document]
//  6. fallback → IntentSpec, [document, memory] (the most conservative default)
//
// Rules 2-5 are checked in the order Spec → Revision → Rationale → Procedure.
// The first match wins; a query matching two intents does not double-route.
// Route never returns a nil/empty AssetTypes — the fallback (rule 6) and every
// keyword rule return a non-empty set.
func (r *router) Route(_ context.Context, q KnowledgeQuery) (Intent, []domain.AssetType, error) {
	// Rule 1: explicit AssetTypes declared by the caller win for the type set;
	// the Intent is still keyword-inferred (the policy depends on intent, not
	// on which ports were called). §4.2 rule 1.
	if len(q.AssetTypes) > 0 {
		intent := r.inferIntent(q.Query)
		types := append([]domain.AssetType(nil), q.AssetTypes...)
		return intent, dedupAssetTypes(types), nil
	}

	// Rules 2-5: keyword inference picks BOTH the intent and the type set.
	// First match wins (Spec → Revision → Rationale → Procedure). §4.2.
	switch {
	case r.cfg.Spec.matches(q.Query):
		return IntentSpec, []domain.AssetType{domain.AssetTypeDocument}, nil
	case r.cfg.Revision.matches(q.Query):
		return IntentRevision, []domain.AssetType{domain.AssetTypeCodebase, domain.AssetTypeDocument}, nil
	case r.cfg.Rationale.matches(q.Query):
		return IntentRationale, []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory}, nil
	case r.cfg.Procedure.matches(q.Query):
		return IntentProcedure, []domain.AssetType{domain.AssetTypeSkill, domain.AssetTypeDocument}, nil
	}

	// Rule 6: fallback — spec + memory, the most conservative default. §4.2.
	return IntentSpec, []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory}, nil
}

// inferIntent resolves the intent from keywords when the caller pinned the
// type set (rule 1). The keyword order is Spec → Revision → Rationale →
// Procedure; no match → IntentSpec (the fallback policy, §4.2 rule 6).
func (r *router) inferIntent(query string) Intent {
	switch {
	case r.cfg.Spec.matches(query):
		return IntentSpec
	case r.cfg.Revision.matches(query):
		return IntentRevision
	case r.cfg.Rationale.matches(query):
		return IntentRationale
	case r.cfg.Procedure.matches(query):
		return IntentProcedure
	}
	return IntentSpec
}

// matches reports whether the query contains any keyword (case-insensitive
// substring). Empty keyword set never matches (so a PM config that clears an
// intent's keywords disables that keyword route — the query falls through to
// the next rule or the fallback).
func (ks KeywordSet) matches(query string) bool {
	if len(ks) == 0 {
		return false
	}
	lq := strings.ToLower(query)
	for _, kw := range ks {
		if kw == "" {
			continue
		}
		if strings.Contains(lq, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// dedupAssetTypes removes duplicate types while preserving order (rule 1: the
// caller's declared set might repeat a type; the Broker fans out once per
// type). A stable dedup keeps the caller's ordering for the fan-out.
func dedupAssetTypes(ts []domain.AssetType) []domain.AssetType {
	if len(ts) <= 1 {
		return ts
	}
	seen := make(map[domain.AssetType]bool, len(ts))
	out := ts[:0]
	for _, t := range ts {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
