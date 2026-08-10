# ADR-0001: CodeGraph 引入选型（框架占位，Phase 3 落地）

- Status: proposed
- Date: 2026-08-10
- Deciders: Mora项目架构师 / 交付部署工程师
- Superseds: —
- Phase: Phase 3（YS-97 代码符号 RAG）

## 背景

Mora 知识底座规划了"代码资产"维度（`knowledge_assets.asset_type = codebase`，见 `design-docs/12-human-agent-knowledge-architecture.md` §4.2 / §10），需要外部能力提供**代码符号解析、调用关系、影响面查询**（§16.1 capability 维度）。该能力的具体选型留到 Phase 3，但 Phase 0 必须先固化**决策框架与门禁**：确保 Phase 3 选型时强制走 ADR + lockfile + digest 流程，不得跳过。

本 ADR 仅是**选型框架模板**，不含实际选型决策；Phase 3 必须填全本文件并置为 `accepted` 才可引入。

## 候选对比

> Phase 3 填写。下表为待评估维度，候选人选在 Phase 3 确定时录入。

| 候选 | 语言/运行时 | License | 活跃度 | 维护成本 | 性能/资源 | 合规影响 | 是否需出网 |
|---|---|---|---|---|---|---|---|
| TBD | TBD | TBD | TBD | TBD | TBD | TBD | TBD |

## 决策

> Phase 3 填写。待选型时确定。

## License 合规影响

> Phase 3 填写。必须核对 AGPL/GPL 传染性义务、网络使用条款、商业限制 / 附加条款（commons-clause / BUSL / SSPL 等），并给出与 Mora Apache-2.0 主许可证是否相容的结论。

## 固定基线

> Phase 3 选型后填全，并同步写入 `third-party/lock.json`。

- 组件名: CodeGraph（占位名，实际以选型为准）
- source_url: TBD（Phase 3）
- commit_sha_or_digest: TBD
- version: TBD
- license: TBD
- notice_path: `third-party/NOTICES/CodeGraph.NOTICE`（TBD）
- capability: 代码符号 / 调用 / 影响查询

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| Phase 3 选型时跳过门禁直接引入 | `make third-party-check` 校验 lock.json 中 CodeGraph 条目的 digest 必须非 TBD；CI fail-closed 阻断发布 |
| CodeGraph 引入出网依赖 | 选型优先选可离线/自托管方案；若必须出网，需 ADR 单列"出网必要性"并由架构师审批 |
| 上游 license 变更 | 锁定具体 commit/digest；升级时重新走本 ADR |

## 升级与回退策略

锁定到具体 commit/digest，禁止漂移到浮动 tag。升级需更新本 ADR + `lock.json` 并重跑 `make third-party-check sbom notices`。

## 门禁要求

- [ ] Phase 3 选型后，本 ADR 置为 `accepted` 并填全"固定基线"。
- [ ] `third-party/lock.json` 写入 CodeGraph 条目（非 TBD）。
- [ ] NOTICE 文件副本置于 `third-party/NOTICES/CodeGraph.NOTICE`。
- [ ] `make third-party-check sbom notices` 通过。
