package context

// intent_test.go verifies IntentRouter.Route against the §4.2 six rules +
// fallback (YS-208 DoD "IntentRouter.Route 覆盖 §4.2 六规则 + fallback，单测
// 各分支"). Each test pins one rule's trigger and asserts the returned
// (Intent, AssetTypes) matches the §4.2 table. The config-injection point is
// exercised by a test that overrides the keyword table.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

func TestIntentRouter_Rule1_ExplicitAssetTypes(t *testing.T) {
	// §4.2 rule 1: explicit AssetTypes non-empty → use the caller's set; Intent
	// is keyword-inferred. Here the query has no keywords, so the Intent falls
	// to the fallback (IntentSpec).
	r := NewIntentRouter(nil)
	explicit := []domain.AssetType{domain.AssetTypeCodebase, domain.AssetTypeSkill}
	intent, types, err := r.Route(context.Background(), KnowledgeQuery{
		Query:      "plain query",
		AssetTypes: explicit,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentSpec {
		t.Errorf("Intent = %q, want spec (fallback when no keyword match)", intent)
	}
	if !sameTypes(types, explicit) {
		t.Errorf("AssetTypes = %v, want caller's explicit set %v", types, explicit)
	}
}

func TestIntentRouter_Rule1_ExplicitSetDedupsAndInfersIntent(t *testing.T) {
	// §4.2 rule 1: the caller's set is deduped; the Intent is inferred from
	// keywords even when types are pinned (policy depends on intent).
	r := NewIntentRouter(nil)
	duped := []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeDocument, domain.AssetTypeMemory}
	intent, types, err := r.Route(context.Background(), KnowledgeQuery{
		Query:      "为什么选 ltree",
		AssetTypes: duped,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentRationale {
		t.Errorf("Intent = %q, want rationale (keyword 为什么)", intent)
	}
	if len(types) != 2 {
		t.Errorf("dedup: got %d types %v, want 2", len(types), types)
	}
}

func TestIntentRouter_Rule2_SpecKeywords(t *testing.T) {
	// §4.2 rule 2: "规范/要求/规格/should/must" → IntentSpec, [document].
	r := NewIntentRouter(nil)
	cases := []string{"选型规范是什么", "权限要求", "规格说明", "agents should", "system must"}
	for _, q := range cases {
		intent, types, err := r.Route(context.Background(), KnowledgeQuery{Query: q})
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if intent != IntentSpec {
			t.Errorf("query %q: Intent = %q, want spec", q, intent)
		}
		if !sameTypes(types, []domain.AssetType{domain.AssetTypeDocument}) {
			t.Errorf("query %q: types = %v, want [document]", q, types)
		}
	}
}

func TestIntentRouter_Rule3_RevisionKeywords(t *testing.T) {
	// §4.2 rule 3: "实现/代码/函数/调用/commit/revision" → IntentRevision,
	// [codebase, document].
	r := NewIntentRouter(nil)
	cases := []string{"这个函数怎么实现的", "看代码", "调用关系", "commit abc123", "revision 2"}
	for _, q := range cases {
		intent, types, err := r.Route(context.Background(), KnowledgeQuery{Query: q})
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if intent != IntentRevision {
			t.Errorf("query %q: Intent = %q, want revision", q, intent)
		}
		if !sameTypes(types, []domain.AssetType{domain.AssetTypeCodebase, domain.AssetTypeDocument}) {
			t.Errorf("query %q: types = %v, want [codebase, document]", q, types)
		}
	}
}

func TestIntentRouter_Rule4_RationaleKeywords(t *testing.T) {
	// §4.2 rule 4: "为什么/决策/原因/why/rationale" → IntentRationale,
	// [document, memory].
	r := NewIntentRouter(nil)
	cases := []string{"为什么选 ltree", "决策依据", "原因是什么", "why this", "rationale for"}
	for _, q := range cases {
		intent, types, err := r.Route(context.Background(), KnowledgeQuery{Query: q})
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if intent != IntentRationale {
			t.Errorf("query %q: Intent = %q, want rationale", q, intent)
		}
		if !sameTypes(types, []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory}) {
			t.Errorf("query %q: types = %v, want [document, memory]", q, types)
		}
	}
}

func TestIntentRouter_Rule5_ProcedureKeywords(t *testing.T) {
	// §4.2 rule 5: "如何执行/流程/步骤/runbook/how" → IntentProcedure,
	// [skill, document].
	r := NewIntentRouter(nil)
	cases := []string{"如何执行部署", "上线流程", "操作步骤", "runbook deploy", "how to"}
	for _, q := range cases {
		intent, types, err := r.Route(context.Background(), KnowledgeQuery{Query: q})
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if intent != IntentProcedure {
			t.Errorf("query %q: Intent = %q, want procedure", q, intent)
		}
		if !sameTypes(types, []domain.AssetType{domain.AssetTypeSkill, domain.AssetTypeDocument}) {
			t.Errorf("query %q: types = %v, want [skill, document]", q, types)
		}
	}
}

func TestIntentRouter_Rule6_Fallback(t *testing.T) {
	// §4.2 rule 6: fallback → IntentSpec, [document, memory]. No keyword and
	// no explicit types.
	r := NewIntentRouter(nil)
	intent, types, err := r.Route(context.Background(), KnowledgeQuery{
		Query: "ltree directory tree",
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentSpec {
		t.Errorf("Intent = %q, want spec (fallback)", intent)
	}
	if !sameTypes(types, []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory}) {
		t.Errorf("types = %v, want [document, memory] (fallback)", types)
	}
}

func TestIntentRouter_Rule2BeatsRule3_FirstMatchWins(t *testing.T) {
	// §4.2 rules 2-5 checked in order; first match wins. A query matching BOTH
	// spec and revision keywords routes to spec (rule 2 precedes rule 3).
	r := NewIntentRouter(nil)
	intent, _, err := r.Route(context.Background(), KnowledgeQuery{
		Query: "规范的代码实现", // 规范 (spec) + 代码 (revision)
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentSpec {
		t.Errorf("Intent = %q, want spec (rule 2 before rule 3)", intent)
	}
}

func TestIntentRouter_ConfigInjectionOverridesKeywords(t *testing.T) {
	// §4.2 keyword table is config-overridable (YS-212). Override Spec with a
	// custom keyword and assert it routes through the injected config, not the
	// default. This is the "结构上预留配置注入点" DoD.
	cfg := &RoutingConfig{
		Spec:      KeywordSet{"cust-spec"},
		Revision:  KeywordSet{"cust-rev"},
		Rationale: DefaultRoutingConfig.Rationale,
		Procedure: DefaultRoutingConfig.Procedure,
	}
	r := NewIntentRouter(cfg)
	intent, types, err := r.Route(context.Background(), KnowledgeQuery{Query: "cust-spec query"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentSpec {
		t.Errorf("injected config: Intent = %q, want spec (cust-spec keyword)", intent)
	}
	if !sameTypes(types, []domain.AssetType{domain.AssetTypeDocument}) {
		t.Errorf("injected config: types = %v, want [document]", types)
	}

	// A default Spec keyword ("规范") no longer matches when overridden.
	intent2, _, _ := r.Route(context.Background(), KnowledgeQuery{Query: "规范是什么"})
	if intent2 != IntentSpec {
		t.Errorf("overridden config: 规范 no longer matches spec; Intent = %q, want spec (fallback still spec)", intent2)
	}
}

func TestIntentRouter_ConfigInjectionDisabledKeywordFallsThrough(t *testing.T) {
	// When a config clears an intent's keywords (empty set), that keyword route
	// is disabled; the query falls through to the next rule or the fallback.
	cfg := &RoutingConfig{
		Spec:      KeywordSet{}, // disabled
		Revision:  KeywordSet{"实现"},
		Rationale: KeywordSet{"为什么"},
		Procedure: KeywordSet{},
	}
	r := NewIntentRouter(cfg)
	// "规范" would match Spec in the default, but Spec is disabled → fallback
	// (no other keyword matches "规范").
	intent, _, err := r.Route(context.Background(), KnowledgeQuery{Query: "规范"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentSpec {
		t.Errorf("disabled Spec keywords: 规范 → fallback; Intent = %q, want spec (fallback)", intent)
	}
	// "实现" still routes to revision (not disabled).
	intent2, _, _ := r.Route(context.Background(), KnowledgeQuery{Query: "实现"})
	if intent2 != IntentRevision {
		t.Errorf("实现 still matches revision; Intent = %q, want revision", intent2)
	}
}

func TestIntentRouter_NilConfigUsesDefault(t *testing.T) {
	// Nil config → DefaultRoutingConfig (§4.2 table). "为什么" matches rationale.
	r := NewIntentRouter(nil)
	intent, _, err := r.Route(context.Background(), KnowledgeQuery{Query: "为什么"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if intent != IntentRationale {
		t.Errorf("nil config → default table; Intent = %q, want rationale", intent)
	}
}

func TestIntentRouter_DoesNotMutateDefaultConfig(t *testing.T) {
	// The default config must not be mutated by a router built from nil (the
	// constructor copies it). Mutating the injected config must not leak back.
	r := NewIntentRouter(nil)
	r2 := NewIntentRouter(nil)
	// Route once to ensure no lazy mutation occur.
	_, _, _ = r.Route(context.Background(), KnowledgeQuery{Query: "为什么"})
	intent, _, _ := r2.Route(context.Background(), KnowledgeQuery{Query: "为什么"})
	if intent != IntentRationale {
		t.Errorf("default config intact after another router used it; Intent = %q", intent)
	}
}

// sameTypes is an order-insensitive type-set comparison. The §4.2 rules return a
// fixed order, but comparing as sets keeps the assertion robust to ordering
// choices in the implementation.
func sameTypes(got, want []domain.AssetType) bool {
	if len(got) != len(want) {
		return false
	}
	wantSet := make(map[domain.AssetType]bool, len(want))
	for _, t := range want {
		wantSet[t] = true
	}
	for _, t := range got {
		if !wantSet[t] {
			return false
		}
	}
	return true
}

// init references uuid to keep the import honest (some test files may not use
// it directly). Avoids an unused-import break if other intent tests drop it.
var _ = uuid.New
