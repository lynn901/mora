package eval

// cases_seed.go is the seed case set for the Context Broker eval runner
// (design-docs/19 §9.1). It demonstrates the case format the test engineer
// ([@Mora知识库测试工程师](mention://agent/9066a6d6-f66a-45cb-844e-81203f6cd137))
// fills per intent × asset type: one CaseSet, DatasetTag pinned to a baseline,
// each case carrying intent + asset_type + query + the expected answer set.
//
// This seed is intentionally minimal — at least one case per (intent, type)
// combo of the four built-in intents × four asset types is NOT the goal of the
// seed; the seed lands ONE representative case per intent and ONE per asset
// type so the runner is exercisable end-to-end before the test engineer
// expands to the full 4×4 matrix + expected answers. The §9.1 gate
// (CitationAccuracy ≥ 95%) is enforced against whatever cases are present; an
// empty seed vacuously passes (no gate to fail) — the test engineer's
// expansion is what turns it into a real gate.
//
// Asset ids / version anchors are placeholders (the test engineer pins them to
// the real baseline dataset when authoring the cases; the runner is broker-
// agnostic — it takes a CaseSet, not a fixture path). The WorkspaceID is a
// fixed sentinel so the seed is self-contained.

import (
	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
)

// SeedWorkspaceID is the workspace the seed cases run in. The test engineer
// repins this to the baseline dataset's workspace when authoring the real
// cases; the runner does not assume a specific workspace.
var SeedWorkspaceID = uuid.MustParse("00000000-0000-0000-0000-000000005ee1")

// seedDocID / seedCodeID / seedMemoryID / seedSkillID are placeholder asset ids
// the seed cases' ExpectedCandidates point at. The test engineer repins them to
// the real baseline asset ids. Valid-hex sentinels (the last group carries a
// readable suffix) so uuid.MustParse succeeds at init.
var (
	seedDocID    = uuid.MustParse("00000000-0000-0000-0000-00000000d0c1")
	seedCodeID   = uuid.MustParse("00000000-0000-0000-0000-00000000c0de")
	seedMemoryID = uuid.MustParse("00000000-0000-0000-0000-000000006d6e")
	seedSkillID  = uuid.MustParse("00000000-0000-0000-0000-000000005d11")
)

// SeedCaseSet is the minimal seed case set. It covers:
//   - each of the four intents at least once (spec / revision / rationale /
//     procedure);
//   - each of the four asset types at least once;
//   - the §11.5 three-state contract (authorized-empty / no-results /
//     provider-down);
//   - §7.2 conflict preservation (must-surface relation survives).
//
// The test engineer expands this to the full 4-intent × 4-type matrix with
// real expected answers + version anchors (§9.1 / D11).
var SeedCaseSet = CaseSet{
	DatasetTag: "seed-baseline",
	Cases: []Case{
		// spec intent → document. The Broker routes a spec query to documents
		// (§4.2 rule 2); the seed expects the API-spec doc surfaced with its
		// version anchor.
		{
			Kind: CaseKindNormal, Intent: ctxmod.IntentSpec, AssetType: domain.AssetTypeDocument,
			Query: "API 分页规范要求", WorkspaceID: SeedWorkspaceID,
			ExpectedCandidates: []uuid.UUID{seedDocID},
			ExpectedCitations:  map[uuid.UUID]string{seedDocID: "v1"},
		},
		// revision intent → codebase. The Broker routes a revision query to
		// [codebase, document] (§4.2 rule 3); the seed pins the codebase type
		// so the per-type report is unambiguous.
		{
			Kind: CaseKindNormal, Intent: ctxmod.IntentRevision, AssetType: domain.AssetTypeCodebase,
			Query: "Serve 函数实现 commit", WorkspaceID: SeedWorkspaceID,
			AssetTypes:         []domain.AssetType{domain.AssetTypeCodebase},
			ExpectedCandidates: []uuid.UUID{seedCodeID},
			ExpectedCitations:  map[uuid.UUID]string{seedCodeID: "seed-commit"},
		},
		// rationale intent → memory. The Broker routes a rationale query to
		// [document, memory] (§4.2 rule 4); the seed pins memory.
		{
			Kind: CaseKindNormal, Intent: ctxmod.IntentRationale, AssetType: domain.AssetTypeMemory,
			Query: "为什么选择 BM25+向量 RAG 决策", WorkspaceID: SeedWorkspaceID,
			AssetTypes:         []domain.AssetType{domain.AssetTypeMemory},
			ExpectedCandidates: []uuid.UUID{seedMemoryID},
		},
		// procedure intent → skill. The Broker routes a procedure query to
		// [skill, document] (§4.2 rule 5); the seed pins skill.
		{
			Kind: CaseKindNormal, Intent: ctxmod.IntentProcedure, AssetType: domain.AssetTypeSkill,
			Query: "如何执行 runbook 部署流程", WorkspaceID: SeedWorkspaceID,
			AssetTypes:         []domain.AssetType{domain.AssetTypeSkill},
			ExpectedCandidates: []uuid.UUID{seedSkillID},
		},
		// §11.5 three-state: authorized-empty. The principal has no use on the
		// matching assets → IsEmptyAuthorized. ExpectedCandidates stays empty.
		{
			Kind: CaseKindAuthorizedEmpty, Intent: ctxmod.IntentSpec, AssetType: domain.AssetTypeDocument,
			Query: "机密规范要求", WorkspaceID: SeedWorkspaceID,
		},
		// §11.5 three-state: no-results. The knowledge base genuinely has
		// nothing → IsEmptyNoResults.
		{
			Kind: CaseKindNoResults, Intent: ctxmod.IntentSpec, AssetType: domain.AssetTypeDocument,
			Query: "完全不存在的术语xyz", WorkspaceID: SeedWorkspaceID,
		},
		// §11.5 three-state: provider-down. A typed port faulted →
		// IsProviderDown with DegradedSources. The runner must NOT score the
		// fault as a recall miss.
		{
			Kind: CaseKindProviderDown, Intent: ctxmod.IntentRevision, AssetType: domain.AssetTypeCodebase,
			Query: "codegraph sidecar 故障查询", WorkspaceID: SeedWorkspaceID,
			AssetTypes: []domain.AssetType{domain.AssetTypeCodebase},
		},
		// §7.2 conflict preservation: high-authority candidates conflict; the
		// Broker must keep both side-by-side (must-surface "contradicts").
		{
			Kind: CaseKindConflictPreservation, Intent: ctxmod.IntentSpec, AssetType: domain.AssetTypeDocument,
			Query: "存在冲突的规范要求", WorkspaceID: SeedWorkspaceID,
			ExpectedRelations: []string{"contradicts"},
		},
	},
}
