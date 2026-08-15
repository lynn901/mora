# ADR-0001: CodeGraph 引入选型

- Status: accepted
- Date: 2026-08-15
- Deciders: Mora项目架构师 / 交付部署工程师
- ADR-IDs: —
- Phase: Phase 3（YS-97 代码符号 RAG）
- Supersedes: 本文件原为 Phase 0 选型框架占位（2026-08-10 提出的 `proposed` 模板），Phase 3 填全并置 `accepted`。

## 背景

Mora 知识底座规划了"代码资产"维度（`knowledge_assets.asset_type = codebase`，见 `design-docs/12-human-agent-knowledge-architecture.md` §4.2 / §10），需要外部能力提供**代码符号解析、调用关系、影响面查询**（§16.1 capability 维度）。Phase 0 仅固化决策框架与门禁（ADR + lockfile + digest），不预选实现；Phase 3（YS-97）完成实际选型并落地 Provider 契约。

需求约束（来自 YS-97 验收门禁与 §7.4 / §10.2）：

- 按**固定 commit** 构建代码图，查询结果必须携带 commit / 文件 / 行号 / 符号定位。
- `graph_ref` / `source_tree_ref` 与 commit 一一绑定，查询前校验 `source_tree_hash`；不符 fail closed，不返回错位源码。
- 100% 本地 / 私有化自托管，默认不出网（Mora 整体部署约束）。
- 按 Provider 声明语言分层，不假设所有语言调用解析覆盖率一致；首版影响候选召回率 ≥ 90%，基准仓库定义/调用查询 100% 命中。
- 许可证须与 Mora 主许可证相容，不引入传染性义务。

## 候选对比

| 候选 | 语言/运行时 | License | 活跃度（2026-08-15 探测） | 维护成本 | 性能/资源 | 合规影响 | 是否需出网 |
|---|---|---|---|---|---|---|---|
| **colbymchenry/codegraph**（npm `codegraph` v1.5.0） | C/TS 混合，树游标解析，SQLite 索引 | MIT | ★66.4k，fork 4.2k，最后 push 2026-08-08，近月连续 5 个版本（v1.3.0→v1.5.0） | 中：独立 sidecar 进程，Mora 不导入其内部包 | 预索引 + 增量同步，SQLite 本地索引；单次只读查询目标 ≤1.5s（§13.3 SLO） | MIT 与 Apache-2.0 相容，无传染性 | 否，100% 本地 |
| tree-sitter + 自研调用图 | C lib + Go 绑定 | MIT（tree-sitter）/ Apache-2.0 | 极活跃 | 高：需自建符号/调用/影响面语义层 | 高性能但工程量大 | 相容 | 否 |
| Sourcegraph SCIP/lsif-indexer | Go | Apache-2.0 | 活跃 | 中高：依赖 SCIP 索引产物，需独立索引管线 | 索引产物大，查询需 SCIP 服务 | 相容 | 否 |
| GitHub Stack Graphs | Rust | MIT | 维护趋缓 | 高：需语义栈编译配置 | 高 | 相容 | 否 |

> 数据来源：`gh api repos/colbymchenry/codegraph`（2026-08-15）—— `license.key=mit`、`stargazers_count=66448`、`pushed_at=2026-08-08T02:56:05Z`、`default_branch=main`；release `v1.5.0`（2026-07-21）；commit `c6aaa20358cd6adcd04b87bdef8e5803ad146f3a`（2026-08-08，PGP verified）。仓库自述："Pre-indexed code knowledge graph, auto syncs on code changes... 100% local"。

## 决策

**选定 `colbymchenry/codegraph`（npm `codegraph` 1.5.0，MIT，commit `c6aaa20358cd6adcd04b87bdef8e5803ad146f3a`）作为 CodeGraph Provider 首版实现基线。**

理由：

1. **契约契合度最高**：原生提供 explore/search/files/node/callers/callees/impact 语义，与 §10.2 `CodeGraphProvider` 窄接口一一对应；`Build` 返回 `graph_ref + source_tree_ref + 索引统计`，可直接映射 §10.2 `BuildResult` 字段集。
2. **100% 本地 + MIT**：满足私有化不出网约束；MIT 与 Mora Apache-2.0 主许可证相容，无 AGPL/GPL/SSPL/BUSL 传染性或商业限制。
3. **活跃度足量**：高 star、连续迭代、commit 有 PGP 验签，可锚定到固定 commit 不漂移。
4. **集成形态明确**：作为独立 sidecar 进程接入（§10.1 远端 Provider 模式：mTLS / 短期服务凭证 + 签名 capability），Mora 通过 Provider 端口适配，**不导入其内部包路径**（蓝图 §"sidecar 不保存 Mora 凭据，只接收短期服务凭证和已裁剪的 AuthzContext"）。若后续评估认为内嵌库更优，可在 ADR-0001 修订中重评，不阻塞首版。

**不做整仓 fork**：仅锁定 commit 作为构建基线，Mora 不复制其控制面/CLI；Provider adapter 在 `internal/infra/codegraph/`（蓝图 §目录预留）封装 sidecar 的 HTTP/本地 RPC 调用。

## License 合规影响

- CodeGraph 许可证：**MIT**（`gh api` 返回 `license.spdx_id=MIT`）。MIT 是宽松许可证，仅需保留版权与许可声明，**无 copyleft 传染**，不触发 Mora（Apache-2.0）的义务扩展。
- 无 commons-clause / BUSL / SSPL / "网络使用即分发"等附加条款（仓库 LICENSE 文件为标准 MIT 文本）。
- Mora 以独立进程方式调用 CodeGraph，不静态/动态链接其二进制到 Mora Go 主程序；即便未来以内嵌方式集成，MIT 亦相容。
- NOTICE 义务：分发产物须附带 CodeGraph 的 MIT 声明，已由 `third-party/NOTICES/CodeGraph.NOTICE` 与 `make notices` 聚合流程承载。

## 固定基线

同步写入 `third-party/lock.json`（新增 `code-symbol-graph` capability 条目）：

- 组件名: `codegraph`（npm 包名 / 仓库名）
- source_url: `https://github.com/colbymchenry/codegraph`
- commit_sha: `c6aaa20358cd6adcd04b87bdef8e5803ad146f3a`（merge commit，2026-08-08，PGP verified）
- version: `1.5.0`（npm tag `codegraph@1.5.0`；GitHub release `v1.5.0`，2026-07-21）
- license: `MIT`
- ecosystem: `npm`（sidecar 侧；Go 侧通过 HTTP/本地 RPC 调用，不进 `go.mod`）
- digest_type / digest: 见 `third-party/lock.json` —— npm 包 `codegraph@1.5.0` 的 tarball sha（`npm view codegraph@1.5.0 dist.shasum` 落地时填入），`source_url` 指向固定 commit
- notice_path: `third-party/NOTICES/CodeGraph.NOTICE`
- capability: `code-symbol-graph`（代码符号 / 调用 / 影响查询）

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| Phase 3 选型时跳过门禁直接引入 | 本 ADR 已置 `accepted` 并填全基线；`make third-party-check` 校验 `lock.json` 中 CodeGraph 条目 digest 非 TBD，CI fail-closed 阻断发布 |
| CodeGraph 引入出网依赖 | 选定方案 100% 本地，无需出网；sidecar 与 Mora 同 Docker 网络，不暴露公网 |
| 上游 license 变更 | 锁定到 commit `c6aaa20`，禁止浮动 tag；升级时重新走本 ADR 并更新 lock.json digest |
| 上游 API 破坏性变更 | Provider adapter 在 `internal/infra/codegraph/` 隔离 sidecar 协议；sidecar 版本固定，升级走 ADR 修订 + capability 契约测试回归 |
| 召回率不达标（<90%） | 按 Provider 声明语言分层暴露能力，未通过契约测试的语言不进 MCP 工具；评测集分层报告召回率，首版以基准仓库 100% 命中为硬门禁 |
| 源码树错位 / stale graph | `source_tree_hash` 校验 fail closed；active graph 与固定 Binding 引用版本的源码树不可清理（详见 design-docs/17 §4） |

## 升级与回退策略

锁定到 commit `c6aaa20358cd6adcd04b87bdef8e5803ad146f3a`，禁止漂移到浮动 tag。升级需：更新本 ADR 的 commit/version/digest → 更新 `third-party/lock.json` → 重跑 `make third-party-check sbom notices` → 重跑 capability 契约测试与评测集。回退即切回已锁定的旧 commit（lock.json 保留历史条目可追溯）。

## 门禁要求

- [x] Phase 3 选型后，本 ADR 置为 `accepted` 并填全"固定基线"。
- [x] `third-party/lock.json` 写入 CodeGraph 条目（非 TBD）。
- [x] NOTICE 文件副本置于 `third-party/NOTICES/CodeGraph.NOTICE`。
- [ ] `make third-party-check sbom notices` 通过（研发落地 sidecar 后在 CI 验证；架构交付期 lock.json 条目已就位，digest 字段在 npm 包实测时填入）。

## 参考

- 设计文档：`design-docs/12-human-agent-knowledge-architecture.md` §7.4（Git 与 CodeGraph）、§10.1（远端 Provider 调用约束）、§10.2（CodeGraphProvider 契约）、§16.4（Phase 3 计划）。
- Phase 3 专项设计：`design-docs/17-phase3-codegraph.md`。
- 选型探测证据：`gh api repos/colbymchenry/codegraph`（2026-08-15，license MIT / ★66.4k / pushed 2026-08-08）、`gh api repos/.../commits/c6aaa20`（verified）、`gh api repos/.../releases`（v1.5.0 @ 2026-07-21）。
