# ADR-0002: Skill 包格式基线（agentskills.io / Hermes 参考）

- Status: accepted（参考基线，Phase 0 固定）
- Date: 2026-08-10
- Component: hermes-agent-skills-spec
- Capability: Skill 包格式 profile（`agentskills.io/<spec-version>` 核心结构 + `hermes/*` 兼容扩展）
- Lockfile entry: `third-party/lock.json` → `components[name=hermes-agent-skills-spec]`
- NOTICE: `third-party/NOTICES/hermes-agent.MIT.txt`
- Selection issue: Phase 1 / YS-95（Skill Package Validator）

## 背景

Mora 需要识别、校验与无损存取 Skill 包——不可变归档，根目录 `SKILL.md`，可含
`references/`、`templates/`、`scripts/`、`assets/`（决策书 `design-docs/12-human-agent-knowledge-architecture.md`
§10.5）。Mora **不执行** Skill；其解析、预览、索引和校验路径均不得执行脚本和二进制。

首版支持两个 profile：

- `agentskills.io/<spec-version>`：核心 `SKILL.md` 结构与渐进式资源读取。
- `hermes/*` compatibility profile：保留 `platforms`、`tool/toolset` 条件、配置、环境变量、
  `credential-file` 声明；Mora 只报告 Runtime 需求，不满足。

本 ADR 固定 Hermes Agent 仓库作为**参考实现基线**，不整体 Fork 其控制面和 UI，避免重复
身份、权限、元数据与部署体系（蓝图 §6.3 调研基线 `2026-08-06` 的 `fe3230f1`，MIT）。

## 候选对比

| 候选 | 语言 | License | 活跃度 | 维护成本 | 性能 | 合规影响 |
|------|------|---------|--------|----------|------|----------|
| Hermes Agent | Python/TS | MIT | 中 | 参考取用，低 | 仅格式参考，无运行时 | 无传染性义务 |
| 自研 SKILL.md schema | — | – | – | 高 | – | – |

决策：参考而非自研格式，避免与生态割裂；但 Mora 只取**格式与校验规则**，不取其 Runtime。

## 决策

固定参考基线：

- name: hermes-agent-skills-spec
- version: agentskills.io/<spec-version>
- source_repo_url: https://github.com/nousresearch/hermes-agent
- commit_sha_or_digest: cd4317b449f93ef34aab83a7dbce5ef6eb14684f
- digest_algorithm: git_commit_sha
- license: MIT
- notice_path: third-party/NOTICES/hermes-agent.MIT.txt
- status: reference_baseline_only
- selection_phase: Phase 1 (YS-95) — Skill Package Validator

接入形态：**spec 参考**，不引入可执行代码。Mora 的校验器只做包结构、引用资源、能力声明、
哈希、来源信任与静态规则检查；脚本/二进制作为不可信资源存储与交付，**绝不执行**。

数据流：Mora 控制面保留资产元数据、身份、权限、审核；Hermes 参考只提供字段定义与兼容
映射，不持有 Mora 凭据。

## License 合规影响（AGPL/GPL 传染性义务 / 商业限制）

- License: MIT —— 在 `lock.json` `allowlist` 中。
- 无传染性义务，无商业限制。
- NOTICE 保留义务由 `third-party/NOTICES/hermes-agent.MIT.txt` 满足。
- 仅参考 spec 与格式，未链接其代码，无静态/动态链接顾虑。

## 固定基线

- source_url: https://agentskills.io/
- source_repo_url: https://github.com/nousresearch/hermes-agent
- commit_sha_or_digest: cd4317b449f93ef34aab83a7dbce5ef6eb14684f
- digest_algorithm: git_commit_sha
- license: MIT
- notice_path: third-party/NOTICES/hermes-agent.MIT.txt
- 何时固定: Phase 0（参考基线）；具体 spec-version 在 Phase 1 选型 Validator 时钉死。

## 风险与缓解

- 上游 spec 破坏性变更：Mora 校验器对未知但合法的 frontmatter/资源**无损保存**，不因不
  理解语义而丢弃（决策书 §10.5）。
- 误执行风险：脚本/二进制作为不可信资源；解析/预览/索引/校验路径均不得执行。
- License 变更：lock.json 固定 commit digest；spec 升级时重新核对 license。

## 升级与回退策略

- 升级 spec-version：Phase 1/3 需求触发，重走本 ADR，更新 `commit_sha_or_digest` 与
  `version` 字段为具体 spec-version，跑 `make third-party-check`。
- 回退：lock.json 回滚到上一个固定 digest；本 ADR 标 `superseded` 并新建后续 ADR。
