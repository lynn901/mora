# Mora 人与 Agent 统一知识蓝图

> 文档版本：v0.1 ｜ 状态：讨论稿 ｜ 更新日期：2026-08-09
> 适用读者：产品、架构、后端、前端、Agent 集成与安全团队
> 关联文档：02-system-architecture.md、03-data-model.md、05-rag-pipeline-design.md、06-mcp-server-design.md、10-document-parsing-design.md

---

## 0. 摘要

Mora 的下一阶段定位是：**面向人和 Agent 的受治理知识底座**。

现有 Mora 已经具备协作文档、版本历史、RBAC、全文与向量检索、文档解析和 MCP 工具层。下一阶段不再只解决“如何检索文档”，而是进一步解决：

1. 人类编写的文档、项目代码、Agent 工作记忆和可复用 Skill 如何在同一工作空间中被发现和治理。
2. Agent 如何按身份、任务和上下文预算获得恰当知识，而不是把整个知识库塞进 Prompt。
3. Agent 执行任务产生的有效经验如何经过提炼、审核和版本化，成为后续人和 Agent 可复用的知识。
4. 不同资产如何保留各自结构：文档继续是文档，代码保留符号与调用关系，记忆保留证据和时效，Skill 保留执行与验证规则。

本蓝图采用“**统一资产注册、类型化处理引擎、统一治理、按需交付**”的总体方案。Mora 继续拥有控制面、身份权限、审计和 Mora 原生文档真源；代码图谱、记忆提炼等能力通过可替换引擎接入。

---

## 1. 背景与目标

### 1.1 背景

当前知识通常分散在多个位置：

- 产品需求、设计决策和运行手册存在于文档系统。
- 系统真实行为存在于代码、配置、迁移和测试中。
- 用户偏好、历史约束、失败教训和隐含决策存在于 Agent 会话中。
- 成熟工作流存在于个人经验、提示词、脚本或零散清单中。

Agent 每次进入新会话时往往需要重新读取、重新解释和重新发现这些信息。标准 RAG 能帮助 Agent 找到相似文本，但无法独立回答以下问题：

- 这条信息是否仍然有效，来源是什么？
- 它是正式规范、代码事实，还是未经确认的会话记忆？
- 哪个 Agent 可以使用，应该直接注入还是按需查询？
- 这次任务形成的经验是否值得沉淀为团队资产？

### 1.2 产品目标

- **统一发现**：用户和 Agent 从一个入口发现文档、代码、记忆和 Skill。
- **保留结构**：不同资产使用适合自身的索引和查询方式，不强制压平为文本 Chunk。
- **受控沉淀**：Agent 经验先成为候选，经去重、冲突检查和治理后再发布。
- **身份感知**：检索、读取、绑定和注入均受用户、团队、Agent 与任务权限约束。
- **来源可追溯**：返回内容带来源、版本、时间和证据引用。
- **按需交付**：优先向 Agent 暴露工具与资产目录，只把必要内容放入上下文。
- **私有化优先**：核心能力支持本地部署；外部连接必须显式配置、审计和限制。

### 1.3 非目标

- 不把 Mora 改造成通用聊天客户端或 Agent 编排平台。
- 不在第一阶段拦截所有模型请求或接管完整 Agent Runtime。
- 不把所有会话记录自动发布为团队知识。
- 不用一个统一向量索引替代代码图、全文索引、版本系统和结构化查询。
- 不在蓝图阶段绑定单一 CodeGraph、记忆模型或 Agent 框架实现。

---

## 2. 产品定位与设计原则

### 2.1 产品定位

Mora 是组织知识的治理与交付控制面，也是 Mora 原生文档的 System of Record。它为不同知识资产统一管理身份、权限、版本、治理状态与检索索引：

```text
人类负责创造、校正和确认知识
Agent 负责发现、应用、提炼和反馈知识
Mora 负责保存结构、来源、权限、版本和生命周期
```

### 2.2 核心原则

1. **知识与索引分离**：索引是可重建的派生物，资产及其来源才是事实记录。
2. **证据先于结论**：记忆和生成内容必须能回到会话、文档、代码或工具执行记录。
3. **候选先于发布**：自动提炼内容默认进入候选态，不直接污染正式知识。
4. **权限先于相关性**：先确定可见范围，再进行召回、排序和上下文组装。
5. **按需优于全量注入**：Agent 通过工具逐步读取，避免上下文噪声和权限扩大。
6. **权威与时效显式化**：相关性分数不能替代来源权威、版本和新鲜度。
7. **引擎可替换**：Mora 统一治理，类型引擎通过稳定接口接入。

### 2.3 术语约定

| 术语 | 含义 |
|---|---|
| 权威内容 | 不能仅靠重新索引恢复的原始记录，如 Mora 文档版本、Git commit、会话证据和 Skill 包 |
| 派生索引 | 可由权威内容重建的 FTS、向量、CodeGraph、摘要和关系建议 |
| `authority` | 针对查询意图评估某资产作为回答依据的优先级，不是固定全局等级 |
| `confidence` | 自动提取内容的模型置信或规则置信，不代表内容已被审核 |
| `trust_level` | 对来源身份、维护方式和更新可靠性的治理评级 |
| `task_affinity` | 资产与当前任务目标、范围和已知上下文的匹配程度 |
| Agent Binding | Agent 的固定资产、可发现范围、显式排除项、版本、使用模式和优先级 |
| Context Broker | 在权限和预算内选择工具、资产摘要与少量正文的交付组件 |
| L0 证据 | 未经知识提炼的原始会话片段或工具执行记录 |

---

## 3. 统一知识资产模型

### 3.1 一级资产类型

| 资产类型 | 主要内容 | 权威表示 | 典型查询 |
|---|---|---|---|
| `document` | 产品文档、设计、规范、手册、知识页面 | 版本化 Block / Markdown | 搜索、读取、历史、关系导航 |
| `codebase` | Git 仓库、目录树、符号、调用关系 | Repo + branch + commit；CodeGraph 为派生索引 | 符号、定义、调用方、影响分析 |
| `memory` | 事实、决策、约束、偏好、事件、场景上下文 | 记忆单元 + 原始证据 | 精确召回、时间过滤、场景恢复 |
| `skill` | 可复用流程、触发条件、资源和验证规则 | 版本化 Skill 包 | 发现、读取、绑定、执行准备 |

一级资产类型保持少而稳定。标签、业务类别、来源方式和文件格式作为属性表达，不继续扩张顶层类型。

### 3.2 KnowledgeAsset 注册信息

所有资产在统一注册表中拥有一份控制面记录：

```text
KnowledgeAsset
  id
  workspace_id
  asset_type
  name
  description
  owner_id
  status
  visibility
  source_id
  source_ref
  content_ref
  version
  governance_profile
  confidence
  valid_from / expires_at
  approved_by / approved_at
  last_synced_at / last_used_at
  usage_count
  metadata
  created_at / updated_at
```

`content_ref` 指向类型引擎中的内容或处理记录：文档 ID、代码资产 ID、记忆记录 ID 或 Skill 包版本。它不改变真源归属；例如代码的真源是固定 commit，CodeGraph 只是由该 commit 重建的索引。统一注册表不复制各引擎的完整内容。

### 3.3 KnowledgeSource

来源描述知识如何进入 Mora，以及如何判断其版本与新鲜度：

```text
KnowledgeSource
  id
  workspace_id
  source_type       native | upload | git | url | api | session
  uri
  credential_ref
  sync_policy       manual | scheduled | webhook
  trust_level
  license
  current_revision
  last_synced_at
  last_error
  enabled
```

一个来源可以产生一个或多个资产；资产版本必须记录对应的来源 revision。凭据只保存 Secret 引用，不保存明文。

### 3.4 资产关系

资产之间通过显式关系形成知识网络：

| 关系 | 示例 |
|---|---|
| `derived_from` | 记忆单元来源于某次 Agent 会话 |
| `explains` | 设计文档解释一组代码模块 |
| `implements` | 代码库实现某个产品规范 |
| `supersedes` | 新规范替代旧规范 |
| `contradicts` | 新证据与已有记忆冲突 |
| `uses` | Skill 使用文档、代码库或脚本资源 |
| `related_to` | 普通知识关联 |

关系记录包含创建者、来源、置信度和时间，生成关系与人工关系必须可区分。

---

## 4. 知识生命周期

### 4.1 通用资产状态

```text
draft ──▶ processing ──▶ candidate ──▶ approved ──▶ published
  │            │              │             │            │
  └──────────▶ failed         └──▶ rejected └──────────▶ deprecated
                                                               │
                                                               ▼
                                                           archived
```

- `draft`：资产外壳或尚未提交的人工内容。
- `processing`：正在解析、建图、提炼或索引。
- `candidate`：机器生成或导入完成，等待治理决策。
- `approved`：已通过审核，等待发布或绑定。
- `published`：可按权限被用户和 Agent 使用。
- `deprecated`：仍可追溯，但默认不参与召回。
- `archived`：长期保留，不进入正常查询。
- `failed/rejected`：处理失败或审核未通过。

只有具备发布权限的人工内容可按现有流程从 `draft` 直接发布。导入或自动生成的资产默认进入 `candidate`；仅当工作空间为可信来源配置了明确的治理策略时，才可自动发布。已发布资产必须记录 `governance_profile`、批准人或自动策略以及批准时间；“正式规范”等权威身份由治理策略认定，不由标题、标签或 `published` 状态单独推断。

### 4.2 Agent 记忆沉淀

```text
Agent 会话 / 工具结果
        │
        ▼
原始证据 L0
        │ 提取
        ▼
记忆候选：事实 / 决策 / 约束 / 偏好 / 事件
        │
        ├── 去重：是否已有等价记忆
        ├── 冲突：是否与正式文档或现有记忆冲突
        ├── 时效：是否需要过期时间
        ├── 权限：个人、团队、特定 Agent 或任务
        └── 来源：会话、消息、工具调用和相关资产
        │
        ▼
审核与发布
        │
        ├── 发布为 memory
        ├── 提升为 document
        ├── 提炼为 skill
        └── 拒绝或合并
```

记忆默认是私有候选。团队共享必须是显式动作，且保留原始证据。高风险领域、低置信度内容和与正式知识冲突的内容必须人工审核。

建议的首版治理默认值：

| 记忆类型 | 默认可见性 | 发布要求 | 默认时效 |
|---|---|---|---|
| 事实 | `private` | 团队发布需证据且人工审核 | 可配置；来源变化后重新验证 |
| 决策 | `private` | 必须关联决策者或正式记录 | 长期有效，直到被替代 |
| 约束 | `private` | 必须说明适用范围；冲突时人工处理 | 建议设置复核时间 |
| 偏好 | `private` | 仅 owner 可发布或授权 | 建议设置过期或确认时间 |
| 事件/事故 | `private` | 脱敏后审核；可提升为复盘文档 | 按保留策略归档 |

首版不自动发布团队记忆。未来只有在记忆类型、风险级别、证据完整性和冲突规则均可验证时，才允许工作空间显式开启低风险自动发布。

证据是受治理的原始记录，不因记忆发布而扩大可见范围。读取证据时同时校验记忆使用权限与原证据 ACL；默认只返回支撑结论的最小脱敏片段。读者无权查看原证据时，仍可看到已脱敏引用、证据类型和校验状态，但不能展开原文。

### 4.3 代码资产生命周期

代码资产以 `repo + branch + commit` 为版本锚点：

1. 首次同步拉取指定 revision。
2. 构建文件树、符号表、调用关系和代码摘要。
3. 将构建结果登记为新资产版本。
4. 增量同步时保留旧 revision 的审计信息，更新当前版本。
5. Agent 返回代码结论时必须携带文件、行号和 commit。

代码正文不复制为普通文档；面向人阅读的架构概览可以作为派生文档，并通过 `derived_from` 指向代码版本。

### 4.4 Skill 生命周期

Skill 不只是 Prompt 文本，至少包含：

- 名称、描述和触发边界。
- 输入、输出与前置条件。
- 执行步骤和引用资源。
- 安全限制与禁止事项。
- 验证规则或检查清单。
- 版本、变更说明和维护者。

从会话提炼出的 Skill 先进入候选态；审核通过后才能绑定到团队 Agent。更新 Skill 产生新版本，Agent 绑定可固定版本或跟随已发布最新版。

---

## 5. 目标架构

### 5.1 分层架构

```text
┌──────────────────────────────────────────────────────────────────────┐
│  来源层                                                              │
│  Mora 编辑器 │ 文件上传 │ Git │ URL/API │ Agent 会话 │ 工具执行记录  │
└───────────────────────────────┬──────────────────────────────────────┘
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│  摄取与处理层                                                        │
│  文档解析 │ Wiki 构建 │ CodeGraph │ Memory Distiller │ Skill Extractor│
└───────────────────────────────┬──────────────────────────────────────┘
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│  Mora 控制面                                                         │
│  Asset Registry │ Source │ Version │ Relation │ RBAC │ Audit │ Review │
└───────────────┬────────────────────┬───────────────────┬─────────────┘
                ▼                    ▼                   ▼
       ┌────────────────┐   ┌────────────────┐  ┌────────────────────┐
       │ PostgreSQL/MinIO│   │ Qdrant / FTS  │  │ 类型化引擎存储      │
       │ 权威内容与元数据 │   │ 派生检索索引    │  │ CodeGraph/Skill/... │
       └────────────────┘   └────────────────┘  └────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│  交付层                                                              │
│  Web 工作台 │ MCP Tools/Resources │ Context Broker │ SDK / Connector │
└───────────────────────────────┬──────────────────────────────────────┘
                                ▼
                      用户与各类 Agent Runtime
```

### 5.2 组件职责

| 组件 | 职责 |
|---|---|
| Asset Registry | 统一登记资产身份、状态、来源、版本、可见性和内容引用 |
| Source Manager | 管理连接、同步策略、revision、失败重试和凭据引用 |
| Ingest Orchestrator | 把来源事件路由到正确的类型引擎，并管理任务状态 |
| Document Engine | 复用现有 Block、版本、解析、FTS 和 RAG 能力 |
| CodeGraph Provider | 构建和查询文件、符号、调用及影响关系 |
| Memory Distiller | 从会话和工具记录中提取、去重和合并记忆候选 |
| Skill Registry | 保存 Skill 包、版本、资源和验证规则 |
| Review Service | 审核候选、处理冲突、发布、废弃和提升资产 |
| Agent Binding | 决定 Agent 可使用哪些固定资产、模式和优先级 |
| Context Broker | 根据身份、任务、查询和预算组装工具及少量上下文 |

### 5.3 引擎边界

Mora 定义稳定 Provider 接口，不让控制面依赖具体第三方内部模型：

```text
AuthzContext
  workspace_id
  principal_type / principal_id
  acting_user_id
  agent_id
  allowed_asset_ids
  allowed_actions
  task_id (optional)

CodeGraphProvider
  Build(authz, source, revision) -> assetVersion
  Search(authz, asset, query)
  GetNode(authz, asset, symbol)
  Callers(authz, asset, symbol)
  Callees(authz, asset, symbol)
  Impact(authz, asset, symbol, depth)
  Files(authz, asset, path)

MemoryProvider
  Capture(authz, evidence)
  Extract(authz, evidence) -> candidates
  Recall(authz, query, budget)
  Merge(authz, candidate, existing)

SkillProvider
  Import(authz, package)
  Validate(authz, version)
  Read(authz, asset, version)
```

Provider 的远端调用由 Mora 统一注入 `AuthzContext`。Provider 必须拒绝不在 `allowed_asset_ids` 或 `allowed_actions` 内的请求，且不得接受客户端自行传入的授权范围。Mora 在调用前授权，Provider 在执行前再次校验，形成边界内外两层防护。

---

## 6. 核心数据流

### 6.1 来源摄取与同步

```text
创建 KnowledgeSource
  → 校验地址、凭据和网络策略
  → 生成 ingest task
  → 拉取 revision
  → 按来源内容选择处理引擎
  → 创建或更新类型资产
  → 建立来源与资产关系
  → 构建派生索引
  → 进入候选审核，或按已批准的可信来源策略发布
  → 记录同步与审计结果
```

同步失败不覆盖最后一个可用版本。新版本完成构建并通过校验后再切换为当前版本。

### 6.2 Agent 任务后的记忆沉淀

```text
Agent 完成任务
  → 显式 remember 或 Connector 提交会话片段
  → 保存脱敏后的证据
  → 异步提取记忆与 Skill 候选
  → 与当前资产去重、检测冲突
  → 进入个人或团队审核收件箱
  → 发布、合并、提升或拒绝
  → 更新索引和 Agent 可用资产
```

第一阶段使用显式提交，避免必须代理全部模型流量。自动捕获在来源、隐私和误写策略成熟后再启用。

### 6.3 Agent 检索与使用

```text
Agent 请求
  → 解析用户、Agent、工作空间和可选任务身份
  → 计算可用固定资产与动态可见资产
  → 判断意图：文档 / 代码 / 记忆 / Skill
  → 暴露相关工具或执行一次受控召回
  → 按权限、权威、新鲜度、相关性排序
  → 在 token、条目数和超时预算内返回
  → 附带来源、版本和引用
  → 记录使用与反馈
```

---

## 7. 检索与上下文组装

### 7.1 类型化路由

| 查询意图 | 优先工具 |
|---|---|
| “支付模块的正式行为是什么” | 文档搜索与读取 |
| “修改这个函数会影响哪里” | CodeGraph impact / callers |
| “上次为什么没有重构认证模块” | 记忆召回 + 来源会话 |
| “这个发布流程怎么执行” | Skill 发现与读取 |
| “当前实现是否符合规范” | 文档 + CodeGraph 联合查询 |

类型路由可以由规则和轻量分类器完成。无法确定时先返回资产候选，不直接注入大段内容。

### 7.2 排序约束

最终排序不是单纯向量相似度，应综合：

```text
final_score = relevance
            × authority_weight
            × freshness_weight
            × task_affinity
            × confidence_weight
```

权限是检索前的硬过滤，不参与乘法评分。被废弃、过期或版本不匹配的资产默认不进入结果。

权威顺序随查询意图变化，系统不得只维护一条固定全局排序：

| 查询意图 | 优先依据 | 冲突处理 |
|---|---|---|
| 规范要求是什么 | 当前已发布规范与决策文档 | 同时标记旧版本或实现偏差 |
| 某个仓库 revision 如何实现 | 该 revision 的代码、配置、迁移和测试 | 显示与文档不一致之处 |
| 当时为什么这样决定 | 决策文档、已审核记忆及其证据 | 低置信记忆不得覆盖正式决策 |
| 任务应该如何执行 | 已批准 Skill、Runbook 和当前环境约束 | 版本不匹配时阻断自动执行 |

当高权威资产互相冲突时，系统应返回冲突及各自引用，不静默选择一个答案。
代码资产只能说明所锚定 revision 的静态实现；如果没有部署版本、运行时配置和特性开关等证据，不得据此宣称生产环境的当前行为。

### 7.3 上下文预算

Context Broker 同时限制：

- 最大 token 或字符数。
- 最大资产数量和单资产占比。
- 查询超时。
- 各资产类型配额。
- 重复信息和同源内容去重。

系统优先注入简短资产目录、摘要和工具说明；正文、代码和历史证据由 Agent 按需读取。

### 7.4 可引用结果

所有返回 Agent 的知识条目应包含：

```text
asset_id / asset_type
title
source_ref
version or revision
updated_at
authority / confidence
引用位置：document block、文件行号、会话消息或 Skill 资源
```

Agent 生成结论时可以把这些引用继续传递给用户和审计系统。

---

## 8. 身份、权限与治理

### 8.1 权限主体

现有 `user/group` 扩展为：

- `user`
- `group`
- `agent`
- `service_account`

Agent 必须绑定 owner、workspace 和有效凭据，不允许匿名访问团队资产。

### 8.2 权限动作

在现有 `read/write/admin` 基础上，引入资产治理动作：

| 动作 | 含义 |
|---|---|
| `read` | 用户读取资产 |
| `write` | 修改内容或元数据 |
| `use` | Agent 在推理或工具调用中使用资产 |
| `assign` | 把资产绑定到 Agent |
| `share` | 改变可见范围或添加 ACL |
| `review` | 审核候选和冲突 |
| `admin` | 完整管理能力 |

`read` 不自动蕴含 `use`，避免可供人查看的敏感材料被 Agent 自动带入模型上下文。

### 8.3 可见性

建议统一支持：

- `private`：仅 owner；团队管理员默认也不可读正文。
- `workspace`：工作空间成员按角色使用。
- `restricted`：通过用户、组或 Agent ACL 精确授权。
- `agent`：只允许明确绑定的 Agent 使用。
- `task`：只在指定任务及其参与者范围内有效；仅在 Task 被确认为一等实体后启用。

### 8.4 授权求交规则

用户通过 Agent 使用资产时，有效权限取以下条件的交集：

```text
effective_access = 资产生命周期允许该动作
                 ∩ acting user 的 RBAC/ACL
                 ∩ Agent 的 use 授权与最小 Binding
                 ∩ task 临时范围（启用时）
                 ∩ Provider 能力范围
```

规则优先级遵循“显式拒绝 > 显式允许 > 角色默认 > 默认拒绝”。`visibility` 是粗粒度候选范围，RBAC/ACL 是最终授权；Binding 只能缩小 Agent 的可用范围，不能扩大用户权限。撤销任一授权后，相关缓存和派生可见性必须失效或重算。

Binding 可同时包含固定资产和管理员授予的可发现范围，显式排除项优先于两者。解除单个固定资产不代表撤销其在工作空间发现范围中的使用权；如需停止使用，必须添加显式排除项或撤销最终 `use` 授权。新请求每次执行授权决策，不得在撤权后继续使用旧结果。

服务账号以自身身份决策；代表用户执行时必须同时携带 `acting_user_id`，不得借服务账号绕过用户权限。团队管理员可以治理团队资产元数据，但不能读取他人的 `private` 正文，除非 owner 显式授权。

### 8.5 前置安全门槛

统一资产能力上线前，必须完成现有资源端点的授权覆盖：目录、评论、权限变更、解析配置和协同入口均需执行资源级 RBAC。否则新增 Agent 主体和自动摄取会放大已有越权面。

### 8.6 隐私与数据最小化

- 会话证据在入库前执行 Secret、凭据和敏感字段检测。
- 自动捕获支持按工作空间、Agent、任务和会话关闭。
- 记忆提炼只提交必要片段，不默认保存完整模型请求和响应。
- 外部 LLM 调用必须通过已批准 Endpoint，并记录出网、模型和数据范围。
- 删除或撤销来源时，明确派生资产的保留、冻结或级联删除策略。

---

## 9. 与当前 Mora 的衔接

### 9.1 直接复用

| 现有能力 | 在蓝图中的角色 |
|---|---|
| `documents` / `document_versions` | Document Asset 的权威内容与版本 |
| Block / Markdown 转换 | 人工文档与解析文档的统一内容格式 |
| 多格式解析 + MinIO | 文件来源的摄取引擎 |
| Valkey Streams | 资产摄取、提炼、同步和重建事件 |
| PostgreSQL FTS + Qdrant | 文档和记忆的混合检索基础 |
| RBAC Engine | 统一资产授权决策基础 |
| Audit / MCP Tool Calls | Agent 使用和资产变更审计 |
| MCP Server | Agent 的按需知识交付入口 |

### 9.2 建议新增模块

```text
internal/module/knowledge/
  asset/          统一资产注册与生命周期
  source/         来源与同步编排
  relation/       资产关系
  review/         候选审核与冲突处理
  binding/        Agent 固定资产绑定
  context/        上下文预算与类型路由

internal/module/memory/
  evidence/       会话与工具证据
  extract/        记忆候选提取
  dedup/          去重与合并
  recall/         记忆召回

internal/infra/codegraph/
  provider.go     稳定 Provider 接口
  http_client.go  可选 sidecar 适配
```

这只是职责边界，不要求第一阶段一次建立全部目录。

### 9.3 建议新增事件

```text
source.sync_requested
source.synced
asset.process_requested
asset.published
asset.deprecated
memory.capture
memory.extract
memory.reviewed
skill.validate
codebase.rebuild
permission.change
```

所有异步事件使用全局 `event_id` 幂等，并携带 workspace、asset、source、revision 和 actor 信息。

### 9.4 第三方能力策略

TencentDB Agent Memory 提供了有价值的参考实现，尤其是统一 Asset、Agent Binding、记忆分层、按需 Knowledge Tools 和 CodeGraph。其许可证为 MIT，可用于原型与实现参考。

Mora 不整体 Fork 其控制面和 UI，避免重复身份、权限、元数据与部署体系。适合优先验证的复用边界是 CodeGraph 或 Knowledge Service sidecar；MemoryCore 与 Proxy 保持可替换参考，不成为 Mora 的强耦合前置依赖。

---

## 10. 产品体验蓝图

### 10.1 工作空间导航

Mora 工作台保留“内容优先”的布局，在现有目录、搜索、权限和历史基础上逐步增加：

- **知识来源**：管理文件、Git、URL/API 和会话来源及同步状态。
- **记忆收件箱**：审核事实、决策、约束和 Skill 候选。
- **代码库**：查看仓库版本、文件树、符号和影响关系。
- **Agent 配装**：为 Agent 绑定资产、使用模式、优先级和固定版本。

这些能力应作为工具面板和管理视图存在，不把编辑器变成资产管理仪表盘。

### 10.2 资产详情

所有资产详情页共享一组治理信息：

- 名称、类型、owner、状态和可见范围。
- 来源、revision、同步时间和处理状态。
- 当前版本与历史版本。
- 已绑定 Agent 和使用模式。
- 最近使用、引用关系和审计记录。
- 发布、废弃、重新处理和解除绑定操作。

内容查看区域按资产类型变化：文档编辑器、代码浏览器、记忆证据视图或 Skill 文件视图。

### 10.3 记忆审核

审核界面需要让人快速判断：

- Agent 提炼出的结论是什么。
- 原始证据在哪里。
- 是否与现有资产重复或冲突。
- 建议的可见性、有效期和关联项目。
- 应发布为 Memory、Document、Skill，还是合并/拒绝。

---

## 11. Agent 接口蓝图

### 11.1 统一发现

```text
assets_list
assets_search
asset_read
asset_relations
```

这些工具用于发现资产，不替代类型化工具。

### 11.2 文档工具

```text
document_search
document_read
document_versions
document_create_draft
document_update
```

可复用现有 MCP 能力，并统一命名、引用和资产身份。

### 11.3 代码工具

```text
code_search
code_files
code_node
code_callers
code_callees
code_impact
code_status
```

管理操作如添加仓库、同步和删除不通过面向 Agent 的默认工具集暴露。

### 11.4 记忆工具

```text
memory_recall
memory_remember       提交候选，不直接发布
memory_evidence_read  只返回通过原证据 ACL 校验的最小脱敏片段
memory_feedback       useful | incorrect | stale
```

### 11.5 Skill 工具

```text
skill_list
skill_read
skill_resources
skill_propose         提交候选
```

所有工具先通过 Agent Binding 与 RBAC 计算可用资产，再执行类型引擎查询。

---

## 12. 分阶段路线

### Phase 0：安全与契约基线

- 补齐现有资源端点 RBAC 覆盖。
- 明确用户、Agent、service account 的身份传播规范。
- 统一 MCP 工具的引用、审计和错误语义。
- 定义 KnowledgeAsset、KnowledgeSource 与 Provider 契约。
- 实现最小 Agent Binding：固定资产、可发现范围、显式排除项和最终 `use` 授权。
- 决定 Task 是否为一等实体；未决定前不启用 `task` 可见性和临时授权。

**门禁**：建立主体 × 资产类型 × 动作 × 访问路径授权矩阵；直接 ID、搜索、MCP、Provider 和异步索引的越权用例必须 100% 拒绝。撤权后的下一次请求必须同步拒绝，缓存与派生索引的可见性在 60 秒内收敛。

### Phase 1：统一资产与来源

- 实现 Asset Registry、Source Manager 和资产关系。
- 把现有文档注册为 Document Asset，不迁移或复制正文。
- 增加文件、Git 和 URL/API 来源任务模型。
- 建立统一资产列表、状态和详情基础 UI。

**门禁**：集成测试证明每个资产版本可追溯到来源 revision；同步构建失败时，当前版本和查询结果仍指向最后一个可用版本。URL/API/Git 连接器的 SSRF、重定向、私网地址、出网白名单、凭据隔离和审计用例必须全部通过。

### Phase 2：代码知识

- 接入一个 CodeGraph Provider 原型。
- 提供代码来源同步、状态、符号和影响查询。
- 在 MCP 暴露只读代码工具。
- 建立代码结果的 commit、文件和行号引用。

**门禁**：在实现前固化带预期答案的仓库问题集；定义与调用查询必须 100% 命中，影响候选召回率不低于 90%。所有结果必须携带 commit、文件与位置，过期 revision 不得伪装为当前结果。

### Phase 3：Agent 记忆沉淀

- 支持显式 `memory_remember` 和会话导入。
- 保存证据、提取记忆候选、去重与冲突提示。
- 在实现发布流程前确定证据保留周期、删除传播、最小引用片段和证据 ACL。
- 建立个人/团队记忆收件箱与发布流程。
- 支持记忆反馈、过期和废弃。

**门禁**：未经审核的团队记忆不进入 Agent 默认召回范围；每条已发布记忆都能回到证据；删除、过期、冲突和撤权路径均有自动化测试。

### Phase 4：Skill 与 Agent 配装

- 实现 Skill 包、版本和验证。
- 支持从任务记录提议 Skill。
- 在最小 Binding 基础上增加固定版本、注入模式、优先级、批量配装和可视化管理。
- 建立 Agent 资产总览。

**门禁**：同一资产无需复制即可绑定到多个 Agent；Agent 切换后权限、固定版本和 Skill 资源均通过契约测试。添加显式排除项或撤销最终 `use` 授权后，下一次请求必须拒绝该资产。

### Phase 5：Context Broker 与自动路由

- 根据查询意图选择类型化工具。
- 实现上下文预算、权威/时效排序和跨资产去重。
- 在明确授权范围内逐步启用自动捕获和动态资产推荐。
- 通过评测优化召回、引用与成本。

**门禁**：授权泄漏和上下文预算超限必须为 0，引用正确率不低于 95%。评测报告同时覆盖类型化召回、端到端延迟和每条上下文的入选原因，发布前锁定当期数据集的召回与延迟阈值。

---

## 13. 成功指标

### 13.1 产品指标

- 新 Agent 完成项目首次任务所需的上下文准备时间。
- 重复解释同一约束或决策的次数。
- 候选记忆的审核通过率、合并率和拒绝率。
- 资产被 Agent 使用后获得 `useful` 反馈的比例。
- Skill 跨 Agent 复用次数和版本采用率。

### 13.2 检索质量

- Recall@K / nDCG，按文档、代码、记忆、Skill 分开评估。
- 引用正确率与 revision 命中率。
- 冲突内容识别率和过期内容误召回率。
- 权限过滤零泄漏。
- 上下文 token 消耗和端到端延迟。

### 13.3 系统指标

- 来源同步成功率与平均新鲜度。
- 处理任务积压、重试和 dead-letter 数量。
- 各 Provider 可用性与降级次数。
- 资产发布、权限变更和 Agent 使用的审计完整率。

---

## 14. 主要风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| 把低质量会话结论当成事实 | 污染团队知识 | 候选态、证据、审核、置信度和冲突检测 |
| 资产模型过度抽象 | 所有类型都被迫走相同逻辑 | 注册信息统一，内容和查询留在类型引擎 |
| 权限在多引擎间漂移 | Agent 越权 | Mora 统一授权；Provider 仅接收已裁剪范围 |
| 代码索引与仓库版本不一致 | 错误影响分析 | commit 锚点、原子版本切换和引用展示 |
| 自动注入造成上下文膨胀 | 成本和质量下降 | 工具优先、预算限制、去重和类型配额 |
| 第三方引擎快速变化 | 集成维护成本高 | Provider Adapter、契约测试和可替换实现 |
| 外部内容过期或许可不清 | 错误或合规风险 | 来源 revision、同步状态、license 和 trust_level |
| 自动捕获包含 Secret | 安全事件 | 脱敏、最小化保存、开关和审计 |

---

## 15. 已确认决策与开放问题

### 15.1 已确认决策

1. Mora 的目标是让人类知识与 Agent 经验持续积累并被共同使用。
2. 一级资产采用 `document/codebase/memory/skill`，不把所有内容压成同一种 Chunk。
3. Mora 拥有统一控制面、权限、审计和 Mora 原生文档真源。
4. 类型化引擎通过 Provider 接口接入，第三方实现可替换。
5. Agent 知识交付以 MCP 按需工具为第一路径，不在早期强依赖全流量代理。
6. Agent 记忆默认先进入候选态，发布必须保留证据和治理状态。
7. 权限过滤先于检索相关性；Agent 使用权限与人类读取权限可以分离。

### 15.2 开放问题

1. Memory 的首批细分类应只覆盖事实/决策/约束，还是同时包含偏好与事件？
2. 未来允许低风险记忆自动发布时，哪些类型、证据和风险规则足以构成可验证门槛？
3. 可发现范围的默认粒度应是工作空间、项目、资产类型，还是组合策略？
4. CodeGraph 首版采用独立 sidecar，还是直接集成某个库到 Go 服务？
5. 文档、代码和记忆冲突时，如何配置工作空间级权威策略？
6. 原始会话证据的默认保留周期和删除传播规则是什么？
7. Skill 的执行由 Agent Runtime 负责，还是 Mora 未来提供受控执行环境？
8. 是否需要把任务建模为一等实体，以承载临时权限、证据和资产产出？

---

## 附录 A：与 TencentDB Agent Memory 的关系

[TencentDB Agent Memory](https://github.com/TencentCloud/TencentDB-Agent-Memory) 与 Mora 在团队记忆、Wiki、CodeGraph、Skill、权限和 Agent 使用层面目标相近。本蓝图调研基线为 2026-08-06 的默认分支提交 `fe3230f1`，仓库许可证文件声明 MIT。值得借鉴的核心设计包括：

- Chat Memory、Skill、Wiki、CodeGraph 四类资产。
- 资产 owner、visibility、status、version 和 Agent Binding。
- 会话记忆分层与异步提炼。
- Wiki/CodeGraph 通过工具按需读取，而不是全量注入。
- 控制面与类型引擎分离。

Mora 的差异化基础是现有协作文档、Block/Markdown、版本历史、目录层级、细粒度 RBAC、私有化 RAG 和 MCP 写能力。蓝图选择吸收上述理念，并围绕 Mora 现有架构原生演进，不复制另一套控制面。

候选复用范围限定为类型引擎或协议适配：优先评估 CodeGraph/Knowledge Service sidecar。资产元数据、身份、权限、审核和 UI 始终由 Mora 控制；sidecar 不保存 Mora 凭据，只接收短期服务凭证和已裁剪的 `AuthzContext`。MemoryCore 和 Proxy 仅作为记忆提炼与上下文注入的设计参考，首版不接入生产链路。

---

## 附录 B：后续设计文档拆分建议

本蓝图确认后，分别产出可实施设计，避免在一份文档中混合全部细节：

1. Knowledge Asset Registry 数据模型与 API。
2. Source Connector 与同步任务设计。
3. CodeGraph Provider 选型与集成设计。
4. Agent Memory 证据、提炼、审核与召回设计。
5. Skill 包格式、版本和 Agent Binding 设计。
6. Context Broker、MCP 工具与上下文预算设计。
7. Agent 身份、`use/assign/share/review` 权限扩展设计。
