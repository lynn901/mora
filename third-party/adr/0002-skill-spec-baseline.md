# ADR-0002: Agent Skills 规格参考基线

- Status: accepted
- Date: 2026-08-10
- Deciders: Mora项目架构师 / 交付部署工程师
- Phase: Phase 0（YS-94 §6.2 固定参考基线）

## 背景

Mora 知识底座规划了 Agent 能力消费模型（§4.3 agents / agent_bindings，§11 Agent Skills），需要以 **Agent Skills 规格作为 Skill 包格式 profile 的参考基线**。这不是引入一个具体运行期第三方代码依赖，而是**固化一份外部规格（spec）的参考版本**：后续 Agent Skills 的引入、解析、校验都以该 spec 版本为准，避免规格漂移。

Phase 0 只固定"参考哪一版规格"这一决策与门禁；实际 Skill 包的引入、解析、消费在对应 Phase 接入。

## 候选对比

| 候选 | 语言/运行时 | License | 活跃度 | 维护成本 | 性能/资源 | 合规影响 |
|---|---|---|---|---|---|---|
| Agent Skills spec（agentskills.io） | 规格文档（非代码） | spec 声明 | 规格维护中 | 低（仅参考规格） | 无（非运行期） | 参考规格，不引入传染性义务 |

## 决策

采用 Agent Skills 规格作为 Skill 包格式 profile 的参考基线，固定到具体 spec 版本号。Mora 不绑定具体 Skill 包仓库，只锁定"规格版本"。

## License 合规影响

参考外部规格本身不构成代码分发，不触发 AGPL/GPL 传染性义务。具体引入某个 Skill **包**时（后续 Phase），按本目录 ADR-0000 模板单独立 ADR，走 lockfile + digest 流程。

## 固定基线

- 组件名: Agent Skills spec
- source_url: https://agentskills.io/<spec-version>
- commit_sha_or_digest: spec 版本号（见 `third-party/lock.json` 的 `version` 字段，spec 为文本规格，digest 用 spec 版本字符串）
- version: <spec-version>（见 `third-party/lock.json`）
- license: spec 声明（见 lock.json；spec 本身许可以其发布页为准）
- notice_path: `third-party/NOTICES/agent-skills-spec.NOTICE`
- capability: Skill 包格式 profile（§16.1 capability 维度）

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| spec 版本演进导致 Skill 包不兼容 | 锁定 spec 版本字符串；升级需更新本 ADR + lock.json |
| 具体 Skill 包引入绕过门禁 | 任何具体 Skill 包引入必须新立 ADR 并写入 lock.json，`make third-party-check` 校验 |
| spec 来源不可达（出网） | spec 为参考规格，本地留存副本于 `third-party/NOTICES/`；运行期不依赖该 URL |

## 升级与回退策略

锁定到具体 spec 版本。升级 spec 版本需更新本 ADR + `third-party/lock.json` 并重跑 `make third-party-check`。

## 门禁要求

- [x] 本 ADR 置为 `accepted`，"固定基线"已填全（spec 版本写入 `lock.json`）。
- [x] `third-party/lock.json` 写入 Agent Skills spec 条目。
- [x] NOTICE 文件副本置于 `third-party/NOTICES/agent-skills-spec.NOTICE`。
- [x] `make third-party-check` 通过。
