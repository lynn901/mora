# ADR-0001: CodeGraph 引入决策（模板 / 待 Phase 3 填充）

- Status: proposed（占位模板，Phase 3 选型时落地为 accepted）
- Date: 2026-08-10
- Component: codegraph
- Capability: 代码符号 / 调用关系 / 影响分析查询（CodeGraph 派生索引）
- Lockfile entry: `third-party/lock.json` → `components[name=codegraph]`
- NOTICE: `third-party/NOTICES/codegraph.MIT.txt`
- Selection issue: Phase 3 / YS-97

## 背景

CodeGraph 为 Mora 的 `codebase` 知识资产类型提供派生索引能力——文件树、符号、调用方、
影响分析。它是**派生索引**而非真源：代码资产的真源是固定 `commit`，CodeGraph 只是由
该 commit 重建的索引（见 `design-docs/11-human-agent-knowledge-blueprint.md` §6）。

Phase 0（本 issue YS-102）**不做** CodeGraph 实际选型，只固化**决策框架与门禁**，确保
Phase 3 选型时必须走 ADR + lockfile + digest 流程，不能跳过（决策书 §6.2）。

## 候选对比

| 候选 | 语言 | License | 活跃度 | 维护成本 | 性能 | 合规影响 |
|------|------|---------|--------|----------|------|----------|
| codegraph 1.5.0（参考基线） | TS/JS | MIT | TBD | TBD | TBD | MIT，无传染性义务 |
| _（Phase 3 时补充其余候选）_ |      |         |        |          |      |          |

Phase 3 选型需在此表补充至少一个备选方案并给出对比理由。

## 决策

**Phase 0：不预选。** 仅在 `third-party/lock.json` 固定参考基线：

- name: codegraph
- version: 1.5.0
- commit_sha_or_digest: c6aaa20
- license: MIT
- notice_path: `third-party/NOTICES/codegraph.MIT.txt`
- status: `reference_baseline_only`
- selection_phase: Phase 3 (YS-97)

Phase 3 必须将本 ADR 的 Status 升级为 `accepted`，填实候选对比、接入形态（独立 sidecar
优先于直接集成为 Go 库，避免运行时污染主进程）与数据流。

## License 合规影响（AGPL/GPL 传染性义务 / 商业限制）

- License: MIT —— 在 `lock.json` `allowlist` 中。
- 无传染性义务，无商业限制。
- NOTICE 保留义务由 `third-party/NOTICES/codegraph.MIT.txt` 满足。

## 固定基线

- source_url: https://www.npmjs.com/package/codegraph/v/1.5.0
- source_repo_url: https://github.com/lynn901/codegraph
- commit_sha_or_digest: c6aaa20
- digest_algorithm: git_commit_short_sha
- license: MIT
- notice_path: third-party/NOTICES/codegraph.MIT.txt
- 何时固定: Phase 0（参考基线）；实际接入 Phase 3（YS-97）。

## 风险与缓解

- 上游突然改 license / 被收购：lock.json 固定 digest，Phase 3 选型时重新核对 license。
- 派生索引与 commit 漂移：CodeGraph 索引必须可由权威 commit 重建，不作为真源（蓝图 §6）。
- 性能/资源：sidecar 形态可独立扩缩容与重启，不拖垮 mora-api 主进程。

## 升级与回退策略

- 升级：Phase 3 选定后，升级需重走本 ADR + `make third-party-check`，更新 digest。
- 回退：lock.json 回滚到上一个固定 digest；本 ADR 标 `superseded` 并新建 ADR-000X。
