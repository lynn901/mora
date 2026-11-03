// Package eval — YS-211 离线评测 case 集（design-docs/19 §9.1）。
//
// 这是测试工程师交付的 case 集 + 预期答案，是 CI 门禁
// `go test ./internal/module/knowledge/context/eval/` 的数据载体。
// runner.go 的 Run() 评分循环由研发实现（见 runner.go）；本文件提供 case
// 数据，使 Run() 落地后即可直接消费。
//
// 设计依据：
//   - §4.1/§4.2 四意图枚举与路由关键词
//   - §5.1 四内置策略的 must_surface_conflicts（contradicts/old_spec/
//     impl_drift/version_mismatch）与默认权重
//   - §7.2 冲突保留：同资产 twin 走 must-surface pin，不同资产冲突并排保留
//   - §9.1 指标按 (intent × asset_type) 分报，不聚合为单一数字
//
// Case 结构沿用 runner.go 的 Case（研发已把 DoD 要求的三个预期字段
// ExpectedCandidates / ExpectedCitations / ExpectedRelations 并入 Case），
// 故本文件的 case literal 直接被 runner.Run() 消费，无需中间适配结构。
//
// case 数据是 slice literal，纯 Go，可在 CI 直接编译运行。预期答案对齐
// 四意图 × 四类型矩阵，含冲突保留场景。固定 UUID 数据集锚点，运行时由
// §9.3 集成测试 seed 到真实 PG；DatasetTag=phase6-v1 供发布前阈值锁定。
package eval

import (
	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// 固定 asset id（评测数据集锚点，运行时由 §9.3 集成测试 seed 到真实 PG）。
// 用固定 UUID 使 ExpectedCandidates / ExpectedRelations 可静态声明。
var (
	// —— spec 意图数据集（当前有效规范文档 + 旧规范 + 实现偏差）——
	specDocV2     = uuid.MustParse("00000000-0000-0000-0000-a00000000002") // 当前有效规范 v2
	specDocV1Old  = uuid.MustParse("00000000-0000-0000-0000-a00000000001") // 旧规范 v1（old_spec）
	specCodeDrift = uuid.MustParse("00000000-0000-0000-0000-a00000000003") // 实现偏差（impl_drift）
	specMemLow    = uuid.MustParse("00000000-0000-0000-0000-a00000000004") // 低置信 memory（rationale 才 surface）

	// —— revision 意图数据集（固定 commit 代码 + 与文档不一致）——
	revCodeCommit  = uuid.MustParse("00000000-0000-0000-0000-b00000000001") // 锚定 commit 的代码
	revDocIncon    = uuid.MustParse("00000000-0000-0000-0000-b00000000002") // doc_inconsistency 文档
	revMemEvidence = uuid.MustParse("00000000-0000-0000-0000-b00000000003") // 审核证据 memory

	// —— rationale 意图数据集（决策文档 + 被替代 memory）——
	ratDocDecision   = uuid.MustParse("00000000-0000-0000-0000-c00000000001") // 决策文档
	ratMemSuperseded = uuid.MustParse("00000000-0000-0000-0000-c00000000002") // 被替代 memory（superseded_memory）
	ratMemEvidence   = uuid.MustParse("00000000-0000-0000-0000-c00000000003") // 审核证据 memory

	// —— procedure 意图数据集（已批准 skill + 版本不匹配）——
	procSkillV2    = uuid.MustParse("00000000-0000-0000-0000-d00000000001") // 已批准 skill v2
	procSkillV1MM  = uuid.MustParse("00000000-0000-0000-0000-d00000000002") // 版本不匹配 skill v1（version_mismatch）
	procDocRunbook = uuid.MustParse("00000000-0000-0000-0000-d00000000003") // runbook 文档
)

// DefaultCaseSets 是 YS-211 交付的评测 case 集：四意图 × 四类型，含冲突保留。
// runner.Run() 消费此集，按 (intent, asset_type) 分报 Recall@K / nDCG /
// CitationAccuracy / P95，不聚合为单一数字（§9.1）。
//
// 矩阵覆盖（§验收标准 DoD 第 1 条）：
//   - spec       → document(主) / code(impl_drift 冲突) / memory / skill
//   - revision   → codebase(主) / document(doc_inconsistency 冲突) / memory / skill
//   - rationale  → document(主) / memory(superseded 冲突) / code / skill
//   - procedure  → skill(主) / document(runbook) / code / memory
//   - 冲突保留专项（同资产 twin pin / 不同资产并排保留，§7.2）
//
//nolint:lll // 预期答案字段对齐设计文档，行长可读性优先
var DefaultCaseSets = []CaseSet{
	{
		DatasetTag: "phase6-v1",
		Cases: []Case{
			// ===== spec 意图：当前有效规范文档为主，must-surface {old_spec, impl_drift} =====
			{
				Intent:      IntentKindSpec,
				Query:       "规范要求 接口版本",
				WorkspaceID: "ws-eval-spec",
				AssetTypes:  []domain.AssetType{domain.AssetTypeDocument},
				// 正向：当前有效 v2 必入选；旧规范 v1 携 old_spec 必须 surface（不静默丢弃）。
				ExpectedIDs:        []string{specDocV2.String(), specDocV1Old.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{specDocV2, specDocV1Old},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: specDocV2, VersionOrRevision: "doc-v2"},
					{AssetID: specDocV1Old, VersionOrRevision: "doc-v1"},
				},
				// spec 不 surface contradicts，故此处无 ExpectedRelations；old_spec 走 ConflictTags。
			},
			{
				Intent:      IntentKindSpec,
				Query:       "规格 实现是否偏差",
				WorkspaceID: "ws-eval-spec",
				AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeCodebase},
				// impl_drift 冲突（code 资产 ConflictTags）必须 surface（spec must-surface 含 impl_drift）。
				ExpectedIDs:        []string{specDocV2.String(), specCodeDrift.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{specDocV2, specCodeDrift},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: specDocV2, VersionOrRevision: "doc-v2"},
					{AssetID: specCodeDrift, VersionOrRevision: "commit-a1b2c3d"},
				},
			},
			{
				Intent:      IntentKindSpec,
				Query:       "规范 相关记忆",
				WorkspaceID: "ws-eval-spec",
				AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
				// spec 默认权重 memory=0.4，低置信 memory（specMemLow）不应入选（spec 不 surface low_confidence）。
				ExpectedIDs:        []string{},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{},
				ExpectedCitations:  []ExpectedCitation{},
				ExpectedRelations:  []ExpectedRelation{},
			},
			{
				Intent:             IntentKindSpec,
				Query:              "规范 skill 配装",
				WorkspaceID:        "ws-eval-spec",
				AssetTypes:         []domain.AssetType{domain.AssetTypeSkill},
				ExpectedIDs:        []string{procSkillV2.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{procSkillV2},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: procSkillV2, VersionOrRevision: "pkg-2.0.0"},
				},
			},

			// ===== revision 意图：固定 commit 代码为主，must-surface {contradicts, impl_drift} =====
			{
				Intent:             IntentKindRevision,
				Query:              "实现 代码 函数调用 commit",
				WorkspaceID:        "ws-eval-revision",
				AssetTypes:         []domain.AssetType{domain.AssetTypeCodebase},
				ExpectedIDs:        []string{revCodeCommit.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{revCodeCommit},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: revCodeCommit, VersionOrRevision: "commit-a1b2c3d"},
				},
			},
			{
				Intent:      IntentKindRevision,
				Query:       "实现 代码与文档不一致",
				WorkspaceID: "ws-eval-revision",
				AssetTypes:  []domain.AssetType{domain.AssetTypeCodebase, domain.AssetTypeDocument},
				// doc_inconsistency 走 ConflictTags，revision must-surface 含 impl_drift（文档与实现不一致）。
				ExpectedIDs:        []string{revCodeCommit.String(), revDocIncon.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{revCodeCommit, revDocIncon},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: revCodeCommit, VersionOrRevision: "commit-a1b2c3d"},
					{AssetID: revDocIncon, VersionOrRevision: "doc-v1"},
				},
			},
			{
				Intent:             IntentKindRevision,
				Query:              "revision 审核证据 memory",
				WorkspaceID:        "ws-eval-revision",
				AssetTypes:         []domain.AssetType{domain.AssetTypeMemory},
				ExpectedIDs:        []string{revMemEvidence.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{revMemEvidence},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: revMemEvidence, VersionOrRevision: "evidence-9f8e"},
				},
			},
			{
				Intent:             IntentKindRevision,
				Query:              "revision skill",
				WorkspaceID:        "ws-eval-revision",
				AssetTypes:         []domain.AssetType{domain.AssetTypeSkill},
				ExpectedIDs:        []string{procSkillV2.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{procSkillV2},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: procSkillV2, VersionOrRevision: "pkg-2.0.0"},
				},
			},

			// ===== rationale 意图：决策文档+memory 为主，must-surface {contradicts, old_spec} =====
			{
				Intent:      IntentKindRationale,
				Query:       "为什么 决策 原因",
				WorkspaceID: "ws-eval-rationale",
				AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
				// 决策文档 + 审核证据 memory 入选；被替代 memory（superseded_memory）走 ConflictTags，必须 surface。
				ExpectedIDs:        []string{ratDocDecision.String(), ratMemEvidence.String(), ratMemSuperseded.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{ratDocDecision, ratMemEvidence, ratMemSuperseded},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: ratDocDecision, VersionOrRevision: "doc-decision-1"},
					{AssetID: ratMemEvidence, VersionOrRevision: "evidence-7a6b"},
					{AssetID: ratMemSuperseded, VersionOrRevision: "evidence-old"},
				},
			},
			{
				Intent:      IntentKindRationale,
				Query:       "为什么 决策 代码依据",
				WorkspaceID: "ws-eval-rationale",
				AssetTypes:  []domain.AssetType{domain.AssetTypeCodebase},
				// rationale 默认权重 code=0.3，低；锚定 commit 代码可作为决策依据但权重低。
				ExpectedIDs:        []string{revCodeCommit.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{revCodeCommit},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: revCodeCommit, VersionOrRevision: "commit-a1b2c3d"},
				},
			},
			{
				Intent:             IntentKindRationale,
				Query:              "为什么 决策 skill",
				WorkspaceID:        "ws-eval-rationale",
				AssetTypes:         []domain.AssetType{domain.AssetTypeSkill},
				ExpectedIDs:        []string{},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{},
			},
			{
				Intent:             IntentKindRationale,
				Query:              "决策 文档",
				WorkspaceID:        "ws-eval-rationale",
				AssetTypes:         []domain.AssetType{domain.AssetTypeDocument},
				ExpectedIDs:        []string{ratDocDecision.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{ratDocDecision},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: ratDocDecision, VersionOrRevision: "doc-decision-1"},
				},
			},

			// ===== procedure 意图：已批准 skill+runbook 为主，must-surface {version_mismatch, impl_drift} =====
			{
				Intent:      IntentKindProcedure,
				Query:       "如何执行 流程 步骤 Runbook",
				WorkspaceID: "ws-eval-procedure",
				AssetTypes:  []domain.AssetType{domain.AssetTypeSkill, domain.AssetTypeDocument},
				// 已批准 skill v2 + runbook 文档入选；版本不匹配 skill v1（version_mismatch）走 ConflictTags，必须 surface。
				ExpectedIDs:        []string{procSkillV2.String(), procDocRunbook.String(), procSkillV1MM.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{procSkillV2, procDocRunbook, procSkillV1MM},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: procSkillV2, VersionOrRevision: "pkg-2.0.0"},
					{AssetID: procDocRunbook, VersionOrRevision: "doc-runbook-1"},
					{AssetID: procSkillV1MM, VersionOrRevision: "pkg-1.0.0"},
				},
			},
			{
				Intent:             IntentKindProcedure,
				Query:              "执行 流程 代码",
				WorkspaceID:        "ws-eval-procedure",
				AssetTypes:         []domain.AssetType{domain.AssetTypeCodebase},
				ExpectedIDs:        []string{revCodeCommit.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{revCodeCommit},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: revCodeCommit, VersionOrRevision: "commit-a1b2c3d"},
				},
			},
			{
				Intent:             IntentKindProcedure,
				Query:              "执行 流程 memory",
				WorkspaceID:        "ws-eval-procedure",
				AssetTypes:         []domain.AssetType{domain.AssetTypeMemory},
				ExpectedIDs:        []string{revMemEvidence.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{revMemEvidence},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: revMemEvidence, VersionOrRevision: "evidence-9f8e"},
				},
			},
			{
				Intent:             IntentKindProcedure,
				Query:              "如何执行 skill 配装",
				WorkspaceID:        "ws-eval-procedure",
				AssetTypes:         []domain.AssetType{domain.AssetTypeSkill},
				ExpectedIDs:        []string{procSkillV2.String(), procSkillV1MM.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{procSkillV2, procSkillV1MM},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: procSkillV2, VersionOrRevision: "pkg-2.0.0"},
					{AssetID: procSkillV1MM, VersionOrRevision: "pkg-1.0.0"},
				},
			},

			// ===== 冲突保留专项（§7.2，DoD 第 1 条"含冲突保留场景"）=====
			// 同资产 twin：一个携 contradicts（Relations），一个携 old_spec（ConflictTags）。
			// revision must-surface {contradicts, impl_drift} → contradicts carrier 被 pin，
			// old_spec carrier 不被 revision pin → 同资产 dedup 保留 pinned 的（contradicts 那个）。
			{
				Intent:      IntentKindRevision,
				Query:       "实现 contradicts twin",
				WorkspaceID: "ws-eval-conflict",
				AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
				// twin 是同一资产 specDocV2 的两个候选版本；contradicts carrier 胜出。
				ExpectedIDs:        []string{specDocV2.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{specDocV2},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: specDocV2, VersionOrRevision: "doc-v2"},
				},
				// contradicts 走 Relations，期望该候选携带 contradicts 指向 specDocV1Old。
				ExpectedRelations: []ExpectedRelation{
					{RelationType: "contradicts", SourceAsset: specDocV2, TargetAsset: specDocV1Old},
				},
			},
			// 不同资产互相 contradicts：两者都保留（并排，§7.2 "不静默选择一个答案"）。
			// ratDocDecision 与 ratMemSuperseded 不同资产，rationale must-surface contradicts → 两者并排。
			{
				Intent:             IntentKindRationale,
				Query:              "为什么 不同资产冲突并排",
				WorkspaceID:        "ws-eval-conflict",
				AssetTypes:         []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
				ExpectedIDs:        []string{ratDocDecision.String(), ratMemSuperseded.String()},
				ExpectedK:          0,
				ExpectedCandidates: []uuid.UUID{ratDocDecision, ratMemSuperseded},
				ExpectedCitations: []ExpectedCitation{
					{AssetID: ratDocDecision, VersionOrRevision: "doc-decision-1"},
					{AssetID: ratMemSuperseded, VersionOrRevision: "evidence-old"},
				},
				ExpectedRelations: []ExpectedRelation{
					{RelationType: "contradicts", SourceAsset: ratDocDecision, TargetAsset: ratMemSuperseded},
				},
			},
		},
	},
}

// CaseCount returns the total case count across all default sets (sanity for
// the DoD "四意图 × 四类型 case 集" coverage check — 16 base + 2 conflict = 18).
func CaseCount() int {
	n := 0
	for _, s := range DefaultCaseSets {
		n += len(s.Cases)
	}
	return n
}
