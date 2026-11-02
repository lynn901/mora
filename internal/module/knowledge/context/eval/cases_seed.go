package eval

import "github.com/lynn901/mora/internal/domain"

// cases_seed.go is the seed baseline case set for the Context Broker eval
// runner (design-docs/19 §9.1). It demonstrates the case format the test
// engineer ([@Mora知识库测试工程师]) fills per intent: one CaseSet carrying
// queries with their expected answer asset ids + version anchors.
//
// This seed is intentionally minimal (a few cases across the four intents) so
// the runner is exercisable end-to-end before the full case set lands. The
// recall (≥ 0.9) / citation (≥ 0.95) / latency (P95 ≤ 2s) gates are enforced
// against whatever cases are present; an empty seed vacuously passes (no gate
// to fail) — the test engineer's expansion is what turns it into a real gate.
//
// Asset ids / version anchors are placeholders ("00000000-...") — the test
// engineer pins them to the real dataset's assets + versions when authoring
// the cases; the runner is broker-agnostic (it takes a CaseSet + a wired
// *Broker, not a fixture path).

// SeedDatasetTag is the dataset tag the seed case set carries (§9.1
// "发布前锁定当期数据集阈值"). A release locks the threshold against a tag.
const SeedDatasetTag = "context-seed-v0"

// SeedBaseline is the seed case set: one case per intent (spec / revision /
// rationale / procedure) targeting the §4.2 type sets, with placeholder
// expected ids + anchors. The runner scores whatever is present; the test
// engineer replaces the placeholders with the real dataset answers.
var SeedBaseline = CaseSet{
	DatasetTag: SeedDatasetTag,
	Cases: []Case{
		// IntentSpec (§4.2 rule 2): "规范" keywords → [document]. Expected a
		// governance-approved spec document, anchored by its version_id.
		{
			Intent:      IntentKindSpec,
			Query:       "规范：分页接口的默认 page_size",
			WorkspaceID: "00000000-0000-0000-0000-000000000001",
			AssetTypes:  []domain.AssetType{domain.AssetTypeDocument},
			ExpectedIDs: []string{"00000000-0000-0000-0000-000000000010"},
			ExpectedVersions: map[string]string{
				"00000000-0000-0000-0000-000000000010": "00000000-0000-0000-0000-000000000100",
			},
		},
		// IntentRevision (§4.2 rule 3): "实现/commit" keywords → [codebase,
		// document]. Expected the code symbol's commit anchor.
		{
			Intent:      IntentKindRevision,
			Query:       "SearchHandler 的实现 commit",
			WorkspaceID: "00000000-0000-0000-0000-000000000001",
			AssetTypes:  []domain.AssetType{domain.AssetTypeCodebase, domain.AssetTypeDocument},
			ExpectedIDs: []string{"00000000-0000-0000-0000-000000000020"},
			ExpectedVersions: map[string]string{
				"00000000-0000-0000-0000-000000000020": "a1b2c3d",
			},
		},
		// IntentRationale (§4.2 rule 4): "为什么/决策" keywords → [document,
		// memory]. Expected a decision doc + the distilled memory evidence.
		{
			Intent:      IntentKindRationale,
			Query:       "为什么选用 RRF 融合而不是只向量检索",
			WorkspaceID: "00000000-0000-0000-0000-000000000001",
			AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
			ExpectedIDs: []string{
				"00000000-0000-0000-0000-000000000030",
				"00000000-0000-0000-0000-000000000031",
			},
			ExpectedVersions: map[string]string{
				"00000000-0000-0000-0000-000000000031": "00000000-0000-0000-0000-000000000310",
			},
		},
		// IntentProcedure (§4.2 rule 5): "如何执行/流程" keywords → [skill,
		// document]. Expected an approved skill package + a runbook doc.
		{
			Intent:      IntentKindProcedure,
			Query:       "如何执行索引重建 runbook",
			WorkspaceID: "00000000-0000-0000-0000-000000000001",
			AssetTypes:  []domain.AssetType{domain.AssetTypeSkill, domain.AssetTypeDocument},
			ExpectedIDs: []string{"00000000-0000-0000-0000-000000000040"},
			ExpectedVersions: map[string]string{
				"00000000-0000-0000-0000-000000000040": "1.2.0",
			},
		},
	},
}
