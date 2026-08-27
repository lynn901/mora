package contextbroker

// intent.go — Intent 枚举（§4.1）+ IntentRouter 实现（§4.2 / §4.3）。
//
// The Intent selects the authority policy (12 §9.5). Four built-in intents map
// 1:1 to the four built-in authority policies (§5.1). First-version routing is
// rule-based (§4.2); model-based classification is a §10 open decision and is
// NOT implemented here.
//
// Routing decides ONLY the policy + asset-type set — never authorization
// (authorization is computed independently by authz.Service, §4.2). The
// keyword tables are part of the versioned policy config (§5), PM-tunable; the
// first version hard-codes them but exposes a RoutingKeywords config knob so a
// later DB-backed config can override the tables without touching the router
// logic (§4.2 "关键词表是版本化配置的一部分，PM 可调，不在代码硬编码").

import (
	"context"
	"errors"
	"strings"

	"github.com/lynn901/mora/internal/domain"
)

// ErrEmptyQuery is returned by Route when the query string is empty after
// trimming AND no explicit AssetTypes were supplied — there is no intent to
// infer and no signal to rank on. The caller (Broker) surfaces this as an
// invalid-request error rather than silently degrading to the fallback (the
// fallback is for an *unmatched* query, not an empty one).
var ErrEmptyQuery = errors.New("contextbroker: empty query with no explicit asset types")

// RoutingKeywords is the versioned, PM-tunable keyword table the rule router
// matches against (§4.2). Each intent has a primary keyword set; the first
// matching intent wins, evaluated in the §4.2 order: spec → revision →
// rationale → procedure. Keywords are matched case-insensitively as substrings
// of the normalized query (lower-cased, whitespace-collapsed).
//
// The first version hard-codes the §4.2 table via DefaultRoutingKeywords; a
// future DB-backed PolicyConfig can supply its own RoutingKeywords through the
// IntentRouter constructor without changing the routing logic — that is the
// "结构上预留配置注入点" the issue requires.
type RoutingKeywords struct {
	Spec      []string // 规范/要求/规格/should/must
	Revision  []string // 实现/代码/函数/调用/commit/revision
	Rationale []string // 为什么/决策/原因/why/rationale
	Procedure []string // 如何执行/流程/步骤/runbook/how
}

// DefaultRoutingKeywords pins the §4.2 keyword table the first version matches
// against. These mirror the design doc verbatim (中文 + 英文关键词) so a query
// in either language routes correctly. PM can override via DB config (YS-212).
func DefaultRoutingKeywords() RoutingKeywords {
	return RoutingKeywords{
		Spec:      []string{"规范", "要求", "规格", "should", "must"},
		Revision:  []string{"实现", "代码", "函数", "调用", "commit", "revision"},
		Rationale: []string{"为什么", "决策", "原因", "why", "rationale"},
		Procedure: []string{"如何执行", "流程", "步骤", "runbook", "how"},
	}
}

// defaultAssetTypes is the per-intent asset-type set (§4.2 right column). The
// router returns these when the caller did not supply explicit AssetTypes; when
// the caller DID supply AssetTypes, rule 1 keeps the caller's set and only the
// Intent is inferred from keywords.
//
// §4.2 rule 6 fallback is IntentSpec + [document, memory] — identical to the
// spec intent's default set, so the no-keyword-match path returns the same
// (IntentSpec, [document, memory]) the spec keyword would. There is no separate
// fallback entry: the fallback IS spec intent, by design.
var defaultAssetTypes = map[Intent][]domain.AssetType{
	IntentSpec:      {domain.AssetTypeDocument, domain.AssetTypeMemory},
	IntentRevision:  {domain.AssetTypeCodebase, domain.AssetTypeDocument},
	IntentRationale: {domain.AssetTypeDocument, domain.AssetTypeMemory},
	IntentProcedure: {domain.AssetTypeSkill, domain.AssetTypeDocument},
}

// fallbackIntent is the intent returned when no keyword matches (§4.2 rule 6).
// It is IntentSpec — the most conservative read — so a query with no keyword
// signal still routes to spec intent with the document+memory type set.
const fallbackIntent Intent = IntentSpec

// RuleRouter is the rule-based IntentRouter (§4.2 first version). It holds a
// RoutingKeywords table (DB-overridable in a later version; hard-coded default
// now) and implements the six-rule ladder.
//
// It is stateless apart from the keyword table; Route is safe to call
// concurrently. The keyword table is copied-on-read so a caller mutating the
// returned slice cannot corrupt the router's state.
type RuleRouter struct {
	keywords RoutingKeywords
}

// NewRuleRouter builds a rule router with the default §4.2 keyword table.
// Callers needing a DB-supplied table (future PM-governed config) can use
// NewRuleRouterWithKeywords.
func NewRuleRouter() *RuleRouter {
	return &RuleRouter{keywords: DefaultRoutingKeywords()}
}

// NewRuleRouterWithKeywords builds a rule router with an explicit keyword
// table. This is the "结构上预留配置注入点" — a later DB-backed config loader
// can construct the router with its own table without changing the routing
// logic. An empty kw falls back to DefaultRoutingKeywords so a misconfigured
// load cannot disable routing.
func NewRuleRouterWithKeywords(kw RoutingKeywords) *RuleRouter {
	if len(kw.Spec) == 0 && len(kw.Revision) == 0 && len(kw.Rationale) == 0 && len(kw.Procedure) == 0 {
		kw = DefaultRoutingKeywords()
	}
	return &RuleRouter{keywords: kw}
}

// Route implements IntentRouter.Route (§4.3). It runs the §4.2 six-rule ladder:
//
//  1. Explicit AssetTypes non-empty → keep caller's type set; infer Intent
//     from query keywords (rules 2-5), or fallback IntentSpec if no keyword
//     matches.
//  2-5. Keyword match → (Intent, default type set for that intent) unless
//       rule 1 already fixed the type set.
//  6. Fallback → IntentSpec + [document, memory].
//
// Route never decides authorization (§4.2). An empty query with no explicit
// types yields ErrEmptyQuery — the fallback is for an unmatched query, not an
// empty one, so the Broker surfaces a real error rather than silently
// returning the conservative default for a malformed request.
func (r *RuleRouter) Route(_ context.Context, q KnowledgeQuery) (Intent, []domain.AssetType, error) {
	query := normalizeQuery(q.Query)
	explicitTypes := q.AssetTypes

	// §4.2 rule 6 fallback fires when the query has no keyword match. But an
	// EMPTY query with no explicit types is a client error, not a fallback case
	// — return ErrEmptyQuery so the Broker surfaces invalid-request rather than
	// pretending a blank query "means spec intent".
	if query == "" && len(explicitTypes) == 0 {
		return "", nil, ErrEmptyQuery
	}

	intent := r.matchIntent(query)
	// §4.2 rule 6: no keyword match → fallback IntentSpec (the most conservative
	// read). The fallback applies to the INTENT regardless of whether the caller
	// supplied explicit AssetTypes — rule 1 says "Intent 按 query 关键词推断",
	// and rule 6 is the fallback when that inference yields nothing. So an
	// unmatched query with explicit types still gets the fallback intent; only
	// the type set comes from the caller.
	if intent == "" {
		intent = fallbackIntent
	}

	// §4.2 rule 1: explicit AssetTypes non-empty → caller's set wins, only the
	// Intent (inferred or fallback above) is returned with it. We copy the slice
	// so the caller cannot mutate the returned type set after the fact.
	if len(explicitTypes) > 0 {
		return intent, cloneAssetTypes(explicitTypes), nil
	}

	// §4.2 rules 2-5: no explicit types → use the matched (or fallback) intent's
	// default type set.
	return intent, cloneAssetTypes(defaultAssetTypes[intent]), nil
}

// matchIntent returns the first intent whose keyword set matches the normalized
// query, evaluated in §4.2 order (spec → revision → rationale → procedure),
// or "" when no keyword matches (caller applies the fallback). First-match
// order is significant: a query containing both "规范" and "代码" routes to
// spec, because §4.2 lists spec before revision.
func (r *RuleRouter) matchIntent(q string) Intent {
	switch {
	case matchesAny(q, r.keywords.Spec):
		return IntentSpec
	case matchesAny(q, r.keywords.Revision):
		return IntentRevision
	case matchesAny(q, r.keywords.Rationale):
		return IntentRationale
	case matchesAny(q, r.keywords.Procedure):
		return IntentProcedure
	default:
		return ""
	}
}

// normalizeQuery lower-cases and collapses whitespace so the keyword match is
// case-insensitive and resilient to extra spacing. It does NOT stem or
// tokenize — §4.2 is a literal substring match, and §10 defers anything
// model-based.
func normalizeQuery(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	// collapse runs of whitespace to a single space so "规范  要求" still
	// matches the "规范" keyword (substring after normalization).
	var b strings.Builder
	b.Grow(len(q))
	inSpace := false
	for _, c := range q {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(c)
	}
	return b.String()
}

// matchesAny reports whether the normalized query contains any of the keywords
// as a substring. Keywords are lower-cased once per call; the keyword table is
// small (≤6 per intent) so this linear scan is cheaper than building a trie.
func matchesAny(q string, keywords []string) bool {
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(q, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// cloneAssetTypes copies the asset-type slice so the caller cannot mutate the
// router's internal default table through the returned slice. The table itself
// is immutable, but slice headers share backing arrays — clone defensively.
func cloneAssetTypes(ts []domain.AssetType) []domain.AssetType {
	if len(ts) == 0 {
		return nil
	}
	out := make([]domain.AssetType, len(ts))
	copy(out, ts)
	return out
}
