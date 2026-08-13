# Mora Context Proxy 设计

> 文档版本：v0.1 ｜ 状态：讨论稿 ｜ 更新日期：2026-08-12
> 适用读者：架构、后端、Agent 集成、安全与运维团队
> 上位架构：12-human-agent-knowledge-architecture.md
> 关联设计：06-mcp-server-design.md、11-human-agent-knowledge-blueprint.md、02-system-architecture.md、03-data-model.md、07-security-observability.md

---

## 0. 摘要与架构决策

Context Proxy 是 Mora 在 Agent 与 LLM 提供商之间插入的可选透明中间层。它解决一个核心问题：Agent 的主动 MCP 工具调用模式依赖 Agent 的"自觉性"，不同 Agent Runtime 对 memory 工具的使用策略差异很大，导致 memory 能力在不自觉的 Agent 上形同虚设。Context Proxy 通过被动注入和被动捕获，把 memory 保存与检索从"Agent 的可选行为"变为"Mora 的保证行为"。

### 0.1 设计约束

1. **Agent 使用自身的 LLM API Key**：Agent 继续用自己的 OpenAI / Anthropic / Ollama / Azure API Key 调用 LLM，Mora 不接管、不替换、不存储 LLM API Key。
2. **路径 Token 模式**：Mora 凭证放在 URL 路径中，LLM API Key 放在 `Authorization` 头中原样透传。兼容只能配置 `base_url` + `api_key` 的 Agent SDK。
3. **不取代 MCP**：Proxy 是 MCP 的可选前置层，两者共存。直连 MCP 的 Agent 走主动调用模式；连 Proxy 的 Agent 走被动注入模式。两种模式共享同一个 Context Broker 和 Authorization Service。
4. **治理红线不变**：被动保存的 Memory 仍走 Candidate -> Review -> Publish 流程，不绕过审核。Evidence 默认 private，不直接进入团队召回。
5. **不接管 Agent Runtime**：Proxy 只做请求改写和响应缓存，不替代 Agent 的工具调用、循环控制和上下文管理。

### 0.2 架构决策

| # | 决策 | 理由 |
|---|---|---|
| 1 | 新增 `cmd/mora-proxy` 进程 | Proxy 的职责（HTTP 反向代理 + 请求改写 + 响应缓存）与 MCP Server（JSON-RPC 协议适配）和 Mora API（控制面）不同，独立进程隔离故障域 |
| 2 | Mora 凭证放 URL 路径 `/p/{token}/` | 兼容只能配 base_url + api_key 的 SDK；token 足够长（128bit+）不可猜测 |
| 3 | LLM API Key 只在 HTTP 转发层流转 | 不进入认证流程、不存储、不日志、不缓存；Mora 只验证 Mora Token |
| 4 | 复用 `api_tokens` 表，新增 `token_purpose` 字段 | 不新建表；Proxy Token 是 `identity_type=agent, token_purpose=proxy` 的 api_token |
| 5 | Memory 注入走 Context Broker 内部 API | 不直连 Qdrant/PG；复用文档 12 §9 的授权、预算和降级机制 |
| 6 | Evidence 捕获异步执行，不阻塞响应 | 对 Agent 透明；失败重试不影响 LLM 响应 |
| 7 | 支持流式（SSE）和非流式两种响应 | 主流 LLM API 都支持 streaming；Proxy 需透传流同时缓存完整内容 |
| 8 | LLM 提供商在 Agent 注册时声明 | Proxy 根据 provider 路由到对应端点；支持 openai/anthropic/ollama/azure/custom |

### 0.3 交付边界

本文定义可进入详细设计和实现的技术骨架：

- Proxy 进程职责与模块边界。
- 路径 Token 认证流程与数据模型。
- 请求侧 Memory + Skill 注入策略与 token 预算。
- 响应侧 Evidence 被动捕获管线。
- LLM 提供商路由表与请求/响应适配。
- 安全边界、流式处理、降级策略。
- API 契约与部署拓扑。

本文不展开每个 REST 字段、完整 SQL、具体记忆提炼 Prompt、前端交互和 Provider 选型细节；这些由后续专项设计承接。

---

## 1. 背景与目标

### 1.1 问题陈述

文档 12 §8.1 明确写道："首版不提供透明 Proxy 自动捕获。任何写入先形成私有 Evidence 和 Candidate，不直接进入团队召回。"蓝图 §1.3 非目标也写了"不拦截所有模型请求或接管完整 Agent Runtime"。

这些约束基于首版的谨慎策略。但实际落地时暴露了一个根本问题：**Agent 的主动调用模式依赖 Agent 的自觉性。**

不同 Agent Runtime 对 MCP 工具的使用策略差异极大：

- 有些 Agent 会在对话开始时主动调用 `memory_recall` 检索相关记忆。
- 有些 Agent 只在 system prompt 被明确要求时才调用。
- 有些 Agent 根本不会在任务结束时调用 `memory_remember` 保存经验。
- 有些 Agent 甚至不接入 MCP，只配一个 `base_url` + `api_key` 直接调 LLM。

结果是：Mora 的 memory 能力在不自觉或不接入 MCP 的 Agent 上形同虚设。

### 1.2 目标

- **被动保证**：Agent 通过 Proxy 接入时，memory 检索和保存一定被触发，不依赖 Agent 的主动调用。
- **API Key 自主**：Agent 继续用自己的 LLM API Key，Mora 不接触、不存储、不替换。
- **SDK 兼容**：兼容只能配置 `base_url` + `api_key` 的主流 Agent SDK（OpenAI SDK、LangChain、LlamaIndex 等）。
- **治理一致**：被动保存的 Memory 和主动保存的走同一条治理管线，不绕过审核。
- **透明无感**：Agent 侧使用体验和直接调 LLM 基本一致，memory 注入和保存对 Agent 透明。
- **与 MCP 共存**：Proxy 不取代 MCP；直连 MCP 的 Agent 不受影响。

### 1.3 非目标

- 不替代 Agent 的工具调用循环和上下文管理。
- 不替代 MCP Server 的协议适配职责。
- 不做 LLM 响应的内容过滤或改写（只做 memory 注入和 evidence 捕获）。
- 不强制所有 Agent 通过 Proxy 接入（Proxy 是可选层）。
- 不存储或管理 Agent 的 LLM API Key。
- 首版不做多模型 fallback 或负载均衡。

---

## 2. 整体架构

### 2.1 逻辑架构

```mermaid
flowchart TB
    Agent["Agent Runtime<br/>配置 base_url 指向 Proxy<br/>api_key 为自己的 LLM Key"]
    Proxy["Mora Context Proxy<br/>路径 Token 认证 + 注入 + 捕获 + 转发"]
    Broker["Context Broker<br/>授权后路由与预算"]
    Authz["Authorization Service"]
    Capture["Evidence Capture<br/>异步脱敏与提炼"]
    LLM["LLM 提供商<br/>OpenAI/Anthropic/Ollama/Azure/Custom"]
    MCP["MCP Server<br/>直连 Agent 的主动工具"]
    API["Mora API<br/>控制面"]
    PG["PostgreSQL"]
    Qdrant["Qdrant"]

    Agent -->|"POST /p/token/v1/chat/completions<br/>Authorization: Bearer sk-xxx"| Proxy
    Agent -->|"MCP tools/call"| MCP
    Proxy -->|"1. 认证: 路径 token hash api_tokens"| PG
    Proxy -->|"2. 注入: POST /internal/v1/knowledge/context"| Broker
    Broker --> Authz
    Broker --> PG
    Broker --> Qdrant
    Proxy -->|"3. 转发: 保留 Authorization 头"| LLM
    LLM -->|"4. 响应返回"| Proxy
    Proxy -->|"5. 异步: Evidence Candidate"| Capture
    Proxy -->|"原样返回响应"| Agent
    MCP --> API
    Broker --> API
```

### 2.2 进程职责

| 进程 | 新增职责 | 明确不负责 |
|---|---|---|
| `mora-proxy` | 路径 Token 认证、Memory + Skill 注入、LLM 请求转发、流式响应透传、Evidence 被动捕获触发 | 直接查 PG/Qdrant、独立计算权限、存储 LLM API Key、替代 Agent 工具调用循环 |
| `mcp-server` | 不变 | 不变 |
| `mora-api` | 新增 Proxy Token 管理端点、Agent LLM provider 配置 | 不变 |
| `knowledge-worker` | 不变（Evidence 捕获仍由 worker 处理） | 不变 |

### 2.3 请求生命周期

```text
Agent 发起 LLM 请求
  |
  v
[1] Proxy 从 URL 路径提取 Mora Token
    -> hash -> api_tokens 查表 -> AuthContext
    -> 失败: 401，不转发
  |
  v
[2] Proxy 从 AuthContext 解析 agent_id, workspace_id
    -> 查 agent_bindings (delivery_mode=inline)
    -> 提取对话上下文 (最后一条 user message + system message)
    -> 调用 Context Broker: POST /internal/v1/knowledge/context
    -> 获得 memory recall 结果 + skill 内容
    -> 注入到 messages (受 token 预算限制)
    -> 失败: 降级为不注入，继续转发，记审计
  |
  v
[3] Proxy 保留原始 Authorization 头
    -> 根据 agent.llm_provider 路由到对应 LLM 端点
    -> 转发改写后的请求
  |
  v
[4] LLM 返回响应 (流式 SSE 或非流式 JSON)
    -> Proxy 透传给 Agent
    -> 同时缓存完整响应内容 (内存中)
  |
  v
[5] 响应结束后，异步触发 Evidence Capture
    -> 提取关键信息 -> 脱敏 -> Evidence (private)
    -> Extraction Provider -> Memory Candidate
    -> 仍走 Candidate -> Review -> Publish
    -> 失败: 重试，不影响已返回的响应
```

---

## 3. 路径 Token 认证模型

### 3.1 URL 格式

```text
POST /p/{proxy_token}/v1/chat/completions      <- OpenAI 兼容
POST /p/{proxy_token}/v1/messages               <- Anthropic 兼容
POST /p/{proxy_token}/v1/embeddings             <- OpenAI embeddings（P2）
```

`proxy_token` 是 Mora 为每个 Agent 生成的 128bit+ 随机字符串（base64url 编码，约 22 字符）。它不是 `agent_id`（后者是 UUID，可被枚举），而是一个不可猜测的 bearer token，只存 SHA-256 hash。

路径中 `/v1/` 之后的部分原样透传给 LLM 提供商，Proxy 不解析也不改写 LLM API 的业务路径。

### 3.2 Agent 配置

Agent 在 Mora 注册时需要声明 LLM 提供商信息，Mora 生成 proxy_token 返回给 Agent（明文，只此一次）：

```text
Agent SDK 配置：
  base_url = https://mora-proxy.example.com/p/{proxy_token}/v1
  api_key  = sk-xxxx                          <- Agent 自己的 LLM API Key
  model    = gpt-4o / claude-sonnet-4 / qwen2.5 / 等
```

对于不需要 API Key 的 LLM（如本地 Ollama），Agent 侧 `api_key` 可以填任意占位值，Proxy 转发时原样传递 Authorization 头，Ollama 会忽略它。

### 3.3 认证流程

```text
1. 从 URL 路径 /p/{token}/ 提取 token 原文
2. HashToken(token) = SHA-256(token) -> hex string
3. TokenStore.Lookup(ctx, hash) -> TokenRecord
4. 验证: rec != nil && rec.IsValid(now)
     -> rec.token_purpose == "proxy"
     -> rec.identity_type == "agent"
5. 构建 AuthContext（复用现有 auth.AuthContext 结构）
6. 从 AuthContext.IdentityID 获取 agent_id
7. 失败: 返回 HTTP 401，不转发请求
```

认证流程复用现有 `auth.TokenStore` 接口和 `auth.HashToken` 函数。Proxy 侧的认证中间件和 MCP 的 `AuthMiddleware` 结构相同，区别只在 token 来源（URL 路径 vs Authorization 头）。

### 3.4 Proxy Token 生命周期

| 操作 | 触发方 | 行为 |
|---|---|---|
| 创建 | Agent 注册或管理员操作 | 生成 128bit 随机 token，存 hash，返回明文（只此一次） |
| 撤销 | 管理员 / Agent owner | 写 `revoked_at`，下一次请求同步拒绝 |
| 过期 | 到期自动 | `expires_at` 到期后拒绝 |
| 轮换 | 管理员 / Agent owner | 撤销旧 token + 创建新 token，返回新明文 |
| 查询 | 管理员 | 列出 token 的 prefix、创建时间、状态（不含明文和 hash） |

一个 Agent 可以持有多个 Proxy Token（多设备、多环境、灰度切换）。撤销一个 Token 不影响同一 Agent 的其他 Token。

### 3.5 与 MCP Token 的关系

Proxy Token 和 MCP Token 都是 `api_tokens` 表中的记录，区别在于：

| 属性 | MCP Token | Proxy Token |
|---|---|---|
| `token_purpose` | `mcp` | `proxy` |
| `identity_type` | `user` / `agent` / `service_account` | `agent` |
| `scope` | `read` / `readwrite` | `proxy`（新 scope 值） |
| 认证来源 | `Authorization` 头 | URL 路径 `/p/{token}/` |
| 用途 | MCP JSON-RPC 工具调用 | LLM 请求代理 |

一个 Agent 可以同时持有 MCP Token 和 Proxy Token：用 MCP Token 直连 MCP Server 做主动工具调用，用 Proxy Token 通过 Proxy 做被动 memory 注入。两种模式可以混用。

---

## 4. 数据模型

### 4.1 现有表扩展

#### `api_tokens` 新增字段

```sql
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS token_purpose TEXT NOT NULL DEFAULT 'mcp';
-- 取值: 'mcp' | 'proxy'
-- 'mcp': 用于 MCP Server JSON-RPC 调用（现有行为）
-- 'proxy': 用于 Context Proxy LLM 请求代理
```

`token_purpose = 'proxy'` 的记录约束：
- `identity_type` 必须为 `'agent'`。
- `scope` 必须为 `'proxy'`。
- 不继承现有 `read` / `readwrite` scope 语义。

#### `agents` 新增字段

```sql
ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_provider TEXT;
-- 取值: 'openai' | 'anthropic' | 'ollama' | 'azure' | 'custom'
-- NULL: Agent 不通过 Proxy 接入，或 provider 未配置

ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_base_url TEXT;
-- custom 时必填；其他 provider 有默认值，可为 NULL
-- 示例: 'https://api.openai.com' / 'http://localhost:11434'
```

### 4.2 LLM 提供商路由表

Provider 路由是配置驱动的，不写死在代码中：

| provider | 默认 base_url | API 格式 | API Key 传递方式 |
|---|---|---|---|
| `openai` | `https://api.openai.com` | OpenAI `/v1/chat/completions` | `Authorization: Bearer sk-xxx` |
| `anthropic` | `https://api.anthropic.com` | Anthropic `/v1/messages` | `x-api-key: sk-ant-xxx` |
| `ollama` | `http://localhost:11434` | OpenAI 兼容 `/v1/chat/completions` | 无需（或忽略） |
| `azure` | Agent 配置的 endpoint | Azure OpenAI 格式 | `api-key: xxx` 头 |
| `custom` | Agent 注册时的 `llm_base_url` | 原样透传路径 | 原样透传 `Authorization` 头 |

`anthropic` 和 `azure` 的 API Key 传递方式与 OpenAI 不同（Anthropic 用 `x-api-key` 头，Azure 用 `api-key` 头）。Proxy 需要在转发时做头部适配：Agent 始终通过 `Authorization: Bearer` 发送自己的 API Key，Proxy 根据 provider 转换为对应格式的头。

对于 `custom` provider，Proxy 不做头部转换，原样透传 `Authorization` 头和请求路径。

对于 `ollama` provider，Proxy 转发时可以去掉 `Authorization` 头（Ollama 忽略它），也可以原样保留（无害）。

### 4.3 迁移文件

```sql
-- migrations/015_context_proxy.up.sql
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS token_purpose TEXT NOT NULL DEFAULT 'mcp';

ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_provider TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_base_url TEXT;

-- 约束: proxy token 必须绑定 agent 身份
ALTER TABLE api_tokens ADD CONSTRAINT chk_proxy_token_agent
    CHECK (token_purpose <> 'proxy' OR identity_type = 'agent');

-- 约束: proxy token 的 scope 必须为 proxy
ALTER TABLE api_tokens ADD CONSTRAINT chk_proxy_token_scope
    CHECK (token_purpose <> 'proxy' OR scope = 'proxy');
```

```sql
-- migrations/015_context_proxy.down.sql
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS chk_proxy_token_scope;
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS chk_proxy_token_agent;
ALTER TABLE agents DROP COLUMN IF EXISTS llm_base_url;
ALTER TABLE agents DROP COLUMN IF EXISTS llm_provider;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS token_purpose;
```

### 4.4 RBAC 扩展

`rbac.Scope` 新增 `proxy` 值：

```text
现有 scope: readonly | readwrite
新增 scope: proxy
```

`proxy` scope 不映射到传统的 read/write 权限。它表示该 Token 仅用于 Context Proxy 的 LLM 请求代理，权限范围由 Agent Binding 和 Authorization Service 联合决定，不受 `CheckWriteScope` 约束。

---

## 5. 请求处理流程

### 5.1 总体流程

Proxy 收到 Agent 请求后，按以下步骤处理：

```text
[1] 认证          路径 token -> hash -> api_tokens -> AuthContext
[2] Agent 解析    AuthContext -> agent_id -> agents 表 -> llm_provider, llm_base_url
[3] Binding 查询  agent_id -> agent_bindings -> delivery_mode=inline 的资产
[4] 上下文提取    从 messages[] 提取 query 上下文
[5] Memory 注入   Context Broker 检索 -> 注入到 messages[]
[6] Skill 注入    绑定的 Skill 内容注入到 system message
[7] 预算校验      Agent 原始 + Mora 注入 <= 模型 context window
[8] 请求转发      根据 provider 路由，保留 Authorization 头
```

### 5.2 上下文提取

Proxy 从 Agent 发来的 `messages[]` 中提取用于 memory recall 的查询上下文：

```text
提取策略（按优先级）：
1. 最后一条 user message 的 content    -> 主要查询信号
2. system message 的 content           -> 任务上下文（如有）
3. 最近 N 条 assistant message 摘要     -> 对话历史上下文（如有）

N 默认为 3，可按 workspace 配置。
提取结果拼接为 ContextBroker query string。
```

Proxy 不修改 Agent 原始的 `messages[]`，而是在注入前先复制一份。如果 memory 注入失败或预算不足，直接用原始 `messages[]` 转发。

### 5.3 Memory 注入策略

#### 注入位置

Memory 和 Skill 内容注入为 `messages[]` 开头的一个 system message，格式如下：

```json
{
  "role": "system",
  "content": "[Mora Knowledge Context]\n\n## Relevant Memories\n{memory_recall_results}\n\n## Active Skills\n{skill_content}\n\n[/Mora Knowledge Context]\n\n以下是你原有的系统指令：\n{original_system_message}"
}
```

如果 Agent 已有 system message，Mora 注入内容前置于原有 system message。如果 Agent 没有 system message，Mora 注入内容作为新的 system message 插入到 `messages[]` 开头。

#### 注入内容标记

注入的 memory 和 skill 内容用 `[Mora Knowledge Context]` / `[/Mora Knowledge Context]` 标签包裹，明确标记为知识上下文数据而非系统指令。这是 Prompt injection 防护的一部分：注入的内容被视为数据，LLM 不应将其中的指令视为系统命令。

#### 注入格式

Memory recall 结果以结构化文本注入，每条 memory 包含：

```text
- [memory_type] statement
  source: {asset_name} v{version} (confidence: {confidence}, valid_until: {expires_at})
```

Skill 内容以可读文本注入，包含 Skill 的 `SKILL.md` frontmatter 摘要和关键指令：

```text
## Skill: {skill_name}
{skill_description}
{skill_key_instructions}
```

### 5.4 Token 预算管理

```text
total_budget   = model_context_window (如 128000 for gpt-4o, 200000 for claude-sonnet-4)
agent_budget   = Agent 发来的 messages[] 的 token 估算
mora_budget    = min(total_budget - agent_budget, total_budget * max_inject_ratio)
                 max_inject_ratio 默认 20%，可按 workspace 配置

if mora_budget <= 0:
    跳过注入，直接转发原始请求
    记审计: "injection_skipped_budget_exhausted"

memory_tokens  = min(memory_recall_result_tokens, mora_budget * 0.7)
skill_tokens  = min(skill_content_tokens, mora_budget * 0.3)
``+
Token 估算使用近似算法（基于字符数的粗略估算），不依赖具体的 tokenizer。精确性不是关键——注入后有余量即可，略微低估比高估安全。

### 5.5 注入降级

| 场景 | 行为 |
|---|---|
| Context Broker 超时（默认 2s） | 跳过 memory 注入，只注入 skill（skill 内容通常可预加载） |
| Context Broker 返回空 | 正常转发，不注入 memory |
| Skill 内容未就绪 | 跳过 skill 注入，只注入 memory |
| Token 预算不足 | 按优先级截断：memory > skill；仍不足则跳过注入 |
| Context Broker 不可用 | 跳过全部注入，直接转发原始请求 |
| Agent 无 inline binding | 不注入，直接转发（Proxy 退化为纯透传代理） |

所有降级都记审计，降级不阻断请求转发。**Proxy 的首要职责是保证 LLM 请求可达，memory 注入是尽力而为的增强。**

---

## 6. Evidence 被动捕获

### 6.1 捕获触发

LLM 响应返回给 Agent 后，Proxy 异步触发 Evidence 捕获。捕获不阻塞响应——Agent 在收到完整响应后即可继续，Evidence 处理在后台进行。

```text
触发时机：
- 非流式响应: 响应 body 完整返回后
- 流式响应:   最后一个 SSE event (data: [DONE]) 之后
```

### 6.2 捕获内容

Proxy 缓存以下信息用于 Evidence 提炼：

| 数据 | 来源 | 用途 |
|---|---|---|
| Agent 请求的 `messages[]`（注入后版本） | Proxy 内存 | 对话上下文 |
| LLM 响应内容 | Proxy 内存 | assistant 回复 |
| Agent 身份 (agent_id, workspace_id) | AuthContext | Evidence owner |
| 注入的 memory 引用列表 | 注入阶段记录 | 上下文关联 |
| 注入的 skill 引用列表 | 注入阶段记录 | 上下文关联 |
| 时间戳和 trace_id | 请求上下文 | 审计和时序 |

Proxy 不缓存 Agent 请求的原始 `messages[]`（注入前版本）——只保留注入后版本，因为注入后版本才是实际发给 LLM 的内容。

### 6.3 捕获管线

```mermaid
flowchart LR
    Cache["Proxy 内存缓存<br/>注入后 messages + LLM 响应"]
    Submit["异步提交<br/>POST /internal/v1/memory/candidates"]
    Sanitize["脱敏处理<br/>Secret/凭据/个人信息检测"]
    Evidence["L0 Evidence<br/>独立 ACL, 默认 private"]
    Extract["Extraction Provider<br/>结构化 Memory Candidate"]
    Review["Review Inbox"]
    Publish["Published Memory"]

    Cache -->|异步| Submit --> Sanitize --> Evidence --> Extract --> Review
    Review -->|approve| Publish
    Review -->|reject| Rejected["Rejected"]
    Review -->|merge| Merge["Merge / supersede"]
```

捕获管线复用文档 12 §8.2 的提炼管线，只是入口不同：

| 入口 | 触发方 | 数据来源 |
|---|---|---|
| `memory_remember` MCP 工具 | Agent 主动调用 | Agent 显式提交的结论和证据引用 |
| **Context Proxy 被动捕获** | Proxy 异步触发 | Proxy 缓存的对话上下文和 LLM 响应 |
| 会话导入 | 用户/管理员选择 | 选定的会话记录 |

三种入口都走同一个 Evidence → Extraction → Candidate → Review → Publish 管线。

### 6.4 脱敏要求

被动捕获的 Evidence 来自完整对话内容，脱敏要求比主动提交更严格：

- **Secret 检测**：API Key、密码、token、连接字符串模式匹配。
- **个人信息检测**：邮箱、手机号、身份证号、IP 地址等 PII。
- **凭据检测**：`password=`, `secret=`, `api_key=` 等 key-value 模式。
- **超范围上下文检测**：非当前 workspace 的资产引用、跨 workspace 泄露内容。

检测命中的内容替换为 `[redacted:{type}]`，不阻断捕获流程。脱敏后的 Evidence 仍可用于记忆提炼，但原始敏感内容不可恢复。

### 6.5 捕获降级

| 场景 | 行为 |
|---|---|
| Extraction Provider 不可用 | 保留 Evidence 为 `pending_extraction`，不写半结构化 Memory |
| 脱敏检测失败 | 保留原始 Evidence 但标记 `sanitization_pending`，不进入提炼 |
| 内存缓存丢失（Proxy 重启） | 丢弃本次捕获，不影响 Agent 已收到的响应 |
| 提交超时 | 重试 3 次，间隔指数退避；仍失败则记审计并丢弃 |
| Agent 无 memory capture binding | 不触发捕获（可通过 binding 配置关闭被动捕获） |

### 6.6 捕获控制

不是所有通过 Proxy 的对话都需要被动捕获。通过 `agent_bindings` 的 `delivery_mode` 和新增的 `capture_mode` 控制行为：

```text
agent_bindings 新增字段:
  capture_mode TEXT DEFAULT 'evidence'
  -- 'evidence':   捕获对话上下文为 Evidence，走提炼管线（默认）
  -- 'none':       不捕获，Proxy 退化为纯注入代理
  -- 'raw':        保留原始会话引用，不提取 Memory（P2，需用户显式选择）
```

Workspace 管理员可以全局关闭被动捕获，或按 Agent 配置。关闭后 Proxy 仍做 memory 注入，但不保存任何对话内容。

---

## 7. LLM 提供商路由

### 7.1 路由决策

Proxy 根据 Agent 注册时声明的 `llm_provider` 决定转发目标：

```text
provider = agent.llm_provider
base_url = agent.llm_base_url OR providerDefaultBaseURL(provider)
api_path = extractFromRequestPath(request_path)  // /v1/chat/completions 等
target_url = base_url + api_path
```

如果 `llm_provider` 为 NULL，Proxy 返回 `409 Conflict`，提示需要先配置 Agent 的 LLM provider。

### 7.2 请求头适配

Agent 始终通过 `Authorization: Bearer <key>` 发送 LLM API Key。Proxy 根据 provider 转换为目标格式：

| provider | 入站头 | 出站头 |
|---|---|---|
| `openai` | `Authorization: Bearer sk-xxx` | `Authorization: Bearer sk-xxx`（不变） |
| `anthropic` | `Authorization: Bearer sk-ant-xxx` | `x-api-key: sk-ant-xxx` + 删除 `Authorization` |
| `ollama` | `Authorization: Bearer xxx` | 删除 `Authorization`（Ollama 忽略） |
| `azure` | `Authorization: Bearer xxx` | `api-key: xxx` + 删除 `Authorization` |
| `custom` | `Authorization: Bearer xxx` | `Authorization: Bearer xxx`（不变） |

Proxy 在头部转换时**不接触 API Key 的值**——只做 header name 的替换，value 原样复制。转换逻辑在 HTTP 反向代理层完成，API Key 不进入 Mora 的认证或审计流程。

### 7.3 Ollama 特殊处理

Ollama 作为本地 LLM 提供商有几个特殊性：

- **无需 API Key**：Ollama 默认不验证 API Key。Agent 侧 `api_key` 可填任意值。
- **OpenAI 兼容端点**：Ollama 支持 `/v1/chat/completions`，请求/响应格式与 OpenAI 兼容。
- **本地端点**：默认 `http://localhost:11434`，部署时可通过环境变量或 Agent 配置覆盖。
- **模型名称**：Ollama 的模型名称（如 `qwen2.5`, `llama3.2`）在请求 body 的 `model` 字段中传递，Proxy 不改写。

对于 Ollama，Proxy 的行为最简单：去掉 `Authorization` 头，原样转发请求和响应。Memory 注入和 Evidence 捕获流程不变。

### 7.4 请求 body 透传规则

Proxy 对请求 body 的改写严格限制：

| 字段 | 是否改写 | 说明 |
|---|---|---|
| `messages[]` | 是 | 注入 memory 和 skill 内容 |
| `model` | 否 | 原样透传 |
| `temperature` | 否 | 原样透传 |
| `max_tokens` | 否 | 原样透传 |
| `stream` | 否 | 原样透传（Proxy 根据此字段决定流式/非流式处理） |
| `tools` / `functions` | 否 | 原样透传（Agent 的工具定义不属于 Mora 管辖） |
| `top_p` / `frequency_penalty` 等采样参数 | 否 | 原样透传 |
| 其他未知字段 | 否 | 原样透传 |

Proxy 只改写 `messages[]`，其余字段原样透传。这保证 Agent 对 LLM 的全部控制权不被 Proxy 侵犯。

### 7.5 响应透传规则

| 响应类型 | 行为 |
|---|---|
| 非流式 JSON 响应 | 完整缓存 body -> 透传给 Agent -> 异步触发 Evidence 捕获 |
| 流式 SSE 响应 | 逐 event 透传给 Agent -> 同时缓存完整内容 -> 流结束后触发 Evidence 捕获 |
| 错误响应 (4xx/5xx) | 原样透传给 Agent -> 不触发 Evidence 捕获 -> 记审计 |
| 超时 | 返回 504 Gateway Timeout -> 记审计 |

Proxy 不改写 LLM 响应内容。响应中的 `usage` 字段（token 计数）原样透传，Proxy 不依赖它做预算计算（预算在请求侧用近似估算完成）。

---

## 8. 安全架构

### 8.1 信任边界

```text
[不可信] Agent Runtime          -- 浏览器和 Agent Runtime 均为不可信客户端（文档 12 §13.1）
[协议边界] Mora Context Proxy   -- 路径 Token 认证 + 请求改写 + 响应缓存
[授权真源] Mora API / Authz     -- 授权决策点
[受限服务] knowledge-worker      -- Evidence 提炼和 Memory Candidate
[独立信任区] LLM 提供商          -- 外部服务，Mora 不信任其安全性
```

Proxy 是协议边界，不是授权真源。它通过内部 API 调用 Mora API 和 Context Broker，不做独立授权决策。

### 8.2 LLM API Key 三条红线

| 红线 | 含义 | 实现 |
|---|---|---|
| **不存储** | LLM API Key 不写入数据库、缓存或对象存储 | API Key 只存在于单个 HTTP 请求的 `Authorization` 头中，请求结束后随内存释放 |
| **不日志** | 审计记录中不含 LLM API Key | `audit.Record` 只记 Mora Token 相关信息；`summarizeParams` 已对 `token`/`secret`/`password`/`api_key` 做 redacted |
| **不验证** | Mora 不校验 LLM API Key 有效性 | Key 无效时 LLM 返回 401，Proxy 原样透传给 Agent |

Proxy 在头部适配（§7.2）时只做 header name 替换，不读取或解析 API Key 的值。API Key 从入站头直接复制到出站头，不经过任何中间变量。

### 8.3 路径 Token 安全

| 威胁 | 防护 |
|---|---|
| Token 被猜测 | 128bit+ 随机生成，不可枚举 |
| Token 在 URL 中泄露 | 要求 TLS；Access log 中对 `/p/{token}/` 路径做脱敏（只保留 prefix） |
| Token 被重放 | 短期过期 + 撤销机制；P2 可加 nonce/timestamp 签名 |
| Token 被窃取后使用 | 审计记录 TokenID + 来源 IP；异常使用告警 |
| Token 被撤销后仍有效 | 撤销写 `revoked_at`，查表时同步拒绝（复用现有 `IsValid` 逻辑） |

### 8.4 Prompt Injection 防护

注入的 memory 和 skill 内容是 Mora 治理后的知识，但仍可能包含注入攻击（例如被恶意篡改的文档内容）。防护措施：

- **内容标记**：注入内容用 `[Mora Knowledge Context]` 标签包裹，明确标记为数据而非指令。
- **来源信任**：只有 `state = published` 的 Memory 和通过校验的 Skill 才被注入。Candidate 和 pending 状态的内容不注入。
- **Agent 自主判断**：Agent Runtime 根据 `compatibility_report` 和自身策略独立决定是否信任注入内容（复用文档 12 §10.5 的原则）。
- **审计可追**：注入的 memory 和 skill 引用列表记入审计，可回溯到来源资产版本。

### 8.5 审计事件

Proxy 新增以下审计事件（复用现有 `audit.Store` 接口）：

```text
proxy.auth.success              路径 Token 认证成功
proxy.auth.failed               路径 Token 认证失败（不含 token 原文）
proxy.inject.success            Memory + Skill 注入成功
proxy.inject.degraded           注入降级（含降级原因）
proxy.inject.skipped            跳过注入（无 binding 或预算不足）
proxy.forward.success           LLM 请求转发成功
proxy.forward.error             LLM 请求转发失败（含 LLM 返回的错误码）
proxy.capture.submitted         Evidence 捕获已提交
proxy.capture.failed            Evidence 捕获提交失败
proxy.token.created             Proxy Token 创建（不含明文）
proxy.token.revoked             Proxy Token 撤销
```

审计参数只记录 ID、动作、结果、耗时和脱敏摘要，不记录 LLM API Key、完整对话内容和 Memory 正文。

---

## 9. 流式响应处理

### 9.1 流式 vs 非流式

Agent 请求中的 `stream` 字段决定响应模式：

| `stream` | Proxy 行为 |
|---|---|
| `true` | SSE 流式透传 + 并行缓存 |
| `false` / 缺省 | 完整 JSON 响应缓存后透传 |

### 9.2 SSE 流式透传

```text
LLM Provider              Proxy                    Agent
    |                        |                        |
    |<-- HTTP POST ----------|<-- HTTP POST ----------|
    |                        | (改写 messages 后转发)  |
    |                        |                        |
    |-- SSE event 1 ------->|-- SSE event 1 ------->|
    |                   (缓存 event 1)               |
    |-- SSE event 2 ------->|-- SSE event 2 ------->|
    |                   (缓存 event 2)               |
    |     ...                |     ...                |
    |-- data: [DONE] ------->|-- data: [DONE] ------->|
    |                   (缓存完成)                    |
    |                        |                        |
    |                        |-- 异步: Evidence 捕获 --|
```

Proxy 对 SSE 流做逐 event 透传，不缓冲完整响应再发。每个 event 到达后立即转发给 Agent，同时追加到内存缓存。流结束后（收到 `data: [DONE]` 或连接关闭），异步触发 Evidence 捕获。

### 9.3 缓存管理

- **非流式**：完整响应 body 缓存在内存中，透传后异步提交 Evidence 捕获。
- **流式**：所有 SSE event 的 `data` 字段缓存到内存 buffer，流结束后拼接为完整响应内容。
- **内存上限**：单个请求的缓存上限默认 256KB（可配置）。超过上限时停止缓存，Evidence 捕获降级为只记审计（"capture_oversized"）。
- **生命周期**：缓存随请求结束释放。Evidence 捕获提交成功后立即释放缓存；提交失败的重试期间保留缓存，重试耗尽后释放。

### 9.4 连接异常处理

| 场景 | 行为 |
|---|---|
| LLM 连接超时 | 返回 504 Gateway Timeout，不触发 Evidence 捕获 |
| LLM 连接中断（流式） | 已转发的 event 保留；返回 502 Bad Gateway；不触发 Evidence 捕获 |
| Agent 主动断开 | 停止转发；如果已有足够缓存则仍触发 Evidence 捕获 |
| Proxy 内存不足 | 降级为不缓存，不触发 Evidence 捕获；保证请求可达 |

---

## 10. 降级与故障处理

### 10.1 降级矩阵

| 组件故障 | 对请求转发的影响 | 对 Memory 注入的影响 | 对 Evidence 捕获的影响 |
|---|---|---|---|
| Context Broker 超时 | 不影响（继续转发） | 跳过注入 | 不影响（使用已缓存的注入结果） |
| Context Broker 不可用 | 不影响 | 跳过全部注入 | 不影响 |
| Qdrant 不可用 | 不影响 | 降级到 FTS（Broker 内部处理） | 不影响 |
| Extraction Provider 不可用 | 不影响 | 不影响 | Evidence 保留为 `pending_extraction` |
| Mora API 不可用 | 不影响（认证用缓存） | 跳过注入 | 捕获失败，重试 |
| PostgreSQL 不可用 | 认证失败（401） | 跳过注入 | 捕获失败 |
| LLM 提供商不可用 | 502/504 返回 Agent | N/A | 不触发捕获 |
| Proxy 自身重启 | 正在处理的请求中断 | N/A | 内存缓存丢失 |

**核心原则：Proxy 的降级策略优先保证 LLM 请求可达。Memory 注入和 Evidence 捕获是尽力而为的增强，任何环节失败都不阻断请求转发。**

### 10.2 认证缓存

为避免每次请求都查 `api_tokens` 表，Proxy 维护短期 Token 认证缓存：

```text
缓存 key:   SHA-256(token)
缓存 value: TokenRecord (含 AuthContext 信息)
缓存 TTL:   60s (可配置)
失效条件:  workspace_authz_revisions 递增时全量失效
             单个 token 撤销时按 key 失效
```

复用文档 12 §5.6 的撤权与缓存机制。Token 撤销后，下一次请求同步拒绝（缓存按 revision 失效）。

### 10.3 并发与限流

| 维度 | 策略 |
|---|---|
| 按 Proxy Token 限流 | 默认 60 req/min，可配 |
| 按 workspace 限流 | 默认 600 req/min，可配 |
| 并发请求上限 | 单 Proxy 实例默认 1000 并发连接 |
| 流式连接超时 | 默认 5 分钟（长对话场景） |
| 非流式请求超时 | 默认 60s（LLM 响应超时） |

限流复用现有 `auth.RateLimiter` 接口，新增 `BucketProxy` bucket。

### 10.4 故障恢复

- Proxy 无状态，重启后立即恢复服务。
- 认证缓存冷启动时退化为每次查表，不阻断请求。
- Evidence 捕获失败的重试由 `knowledge-worker` 承担（通过 Outbox 事件），Proxy 只负责提交。
- 如果 Proxy 持续不可用，Agent 可以配置 fallback base_url 直连 LLM 提供商（由 Agent 自行管理，Proxy 不参与 fallback）。

---

## 11. 模块边界与代码组织

### 11.1 目标目录

```text
cmd/
  mora-proxy/              # 新增进程入口
    main.go

internal/module/proxy/    # 新增模块
  server/
    server.go             # HTTP server, 路由分发
    handler.go            # 请求处理入口
  auth/
    middleware.go         # 路径 Token 认证中间件
    cache.go              # Token 认证缓存
  injector/
    context.go            # 上下文提取
    memory.go             # Memory 注入
    skill.go              # Skill 注入
    budget.go             # Token 预算管理
  capture/
    cache.go              # 响应缓存
    submit.go             # Evidence 提交
    sanitize.go           # 脱敏处理
  llm/
    router.go             # Provider 路由
    forwarder.go          # HTTP 转发
    header.go             # 请求头适配
  streaming/
    sse.go                # SSE 流式透传
    buffer.go             # 流式响应缓存
```

### 11.2 依赖规则

```text
cmd/mora-proxy
  -> internal/module/proxy/*

internal/module/proxy/server
  -> proxy/auth        (认证)
  -> proxy/injector    (注入)
  -> proxy/capture     (捕获)
  -> proxy/llm         (转发)
  -> proxy/streaming   (流式)
  -> mcp/auth           (复用 TokenStore, AuthContext, HashToken)
  -> mcp/moraclient     (复用 MoraClient 调用 Context Broker)
  -> platform/observ    (指标)
  -> platform/rbac      (Scope, IdentityType)

internal/module/proxy/auth
  -> mcp/auth           (复用 TokenStore, AuthContext, HashToken)

internal/module/proxy/injector
  -> mcp/moraclient     (调用 Context Broker 内部 API)
  -> knowledge/context  (Context Broker 端口接口)

internal/module/proxy/capture
  -> mcp/moraclient     (提交 Evidence Candidate)

internal/module/proxy/llm
  -> (无外部模块依赖，纯 HTTP 转发)
```

关键约束：
- Proxy 不直接导入 `repository` 或 `Provider adapter`。
- Proxy 不直接查 PG/Qdrant，所有知识操作通过 `moraclient` 调用 Mora API 内部接口。
- Proxy 复用 `mcp/auth` 的 `TokenStore`、`AuthContext` 和 `HashToken`，不重复实现认证逻辑。
- Proxy 的 `moraclient` 调用复用 MCP Server 的 HTTP client，但使用不同的 internal endpoint。

### 11.3 与现有模块的复用关系

| 现有模块 | Proxy 复用方式 |
|---|---|
| `mcp/auth.TokenStore` | 直接复用，查 `api_tokens` 表 |
| `mcp/auth.AuthContext` | 直接复用，作为 Proxy 请求的认证上下文 |
| `mcp/auth.HashToken` | 直接复用，SHA-256 hash |
| `mcp/moraclient.MoraClient` | 扩展，新增 `PostContext` 和 `PostMemoryCandidate` 方法 |
| `mcp/auth.RateLimiter` | 扩展，新增 `BucketProxy` bucket |
| `platform/observ` | 扩展，新增 Proxy 相关指标 |
| `platform/rbac` | 扩展，新增 `proxy` scope |
| `audit.Store` | 扩展，新增 Proxy 审计事件类型 |

---

## 12. API 契约

### 12.1 控制面 API（Mora API REST）

新增端点：

```text
# Agent LLM provider 配置
PUT    /api/v1/agents/{id}/llm-config
  Body: { "llm_provider": "ollama", "llm_base_url": "http://localhost:11434" }

GET    /api/v1/agents/{id}/llm-config
  -> { "llm_provider": "ollama", "llm_base_url": "http://localhost:11434" }

# Proxy Token 管理
POST   /api/v1/agents/{id}/proxy-tokens
  -> { "token": "mora_xxxxxxxxxxxx", "token_id": "pt-xxx", "prefix": "mora_x" }
  # 明文只返回一次

GET    /api/v1/agents/{id}/proxy-tokens
  -> { "items": [{ "token_id": "pt-xxx", "prefix": "mora_x", "created_at": "...", "status": "active" }] }
  # 不含明文

DELETE /api/v1/agents/{id}/proxy-tokens/{token_id}
  # 撤销 token
```

### 12.2 Proxy 端点（用户面）

```text
# OpenAI 兼容
POST /p/{proxy_token}/v1/chat/completions
  Headers:
    Authorization: Bearer sk-xxxx        # Agent 的 LLM API Key
    Content-Type: application/json
  Body: { "model": "gpt-4o", "messages": [...], "stream": true }

# Anthropic 兼容
POST /p/{proxy_token}/v1/messages
  Headers:
    Authorization: Bearer sk-ant-xxxx    # Agent 的 Claude API Key
    Content-Type: application/json
    anthropic-version: 2023-06-01
  Body: { "model": "claude-sonnet-4-20250514", "messages": [...], "stream": true }
```

### 12.3 内部 API（Proxy -> Mora API）

复用文档 12 §11.2 的内部接口：

```text
POST /internal/v1/knowledge/context       # Context Broker 检索（memory + skill）
POST /internal/v1/memory/candidates       # 提交 Memory Candidate
GET  /internal/v1/agents/{id}             # 查询 Agent LLM provider 配置
```

Proxy 调用内部 API 使用服务身份 + 路径 Token 关联的 delegated context（复用文档 12 §5.1 的调用模式）。`INTERNAL_SERVICE_TOKEN` 不能单独代表最终用户权限。

### 12.4 错误语义

| 场景 | HTTP 状态 | 响应 body |
|---|---|---|
| 路径 Token 无效/过期/撤销 | 401 | `{"error": {"type": "unauthorized", "message": "invalid proxy token"}}` |
| Agent 的 LLM provider 未配置 | 409 | `{"error": {"type": "conflict", "message": "llm_provider not configured"}}` |
| 限流 | 429 | `{"error": {"type": "rate_limited", "retry_after": 60}}` |
| LLM 提供商返回错误 | 原样透传 | 原样透传 LLM 的错误响应 |
| LLM 提供商超时 | 504 | `{"error": {"type": "timeout", "message": "upstream timeout"}}` |
| Proxy 内部错误 | 500 | `{"error": {"type": "internal", "message": "..."}}` |
| 注入降级 | 不影响响应 | 记审计，不返回错误给 Agent |

Proxy 的错误响应格式尽量兼容 OpenAI API 的错误格式（`{"error": {"type": ..., "message": ...}}`），让 Agent SDK 的错误处理逻辑正常工作。

---

## 13. 部署拓扑

### 13.1 Compose 拓扑

默认 Compose 新增 `mora-proxy`：

```text
mora-api
mcp-server
mora-proxy              # 新增
rag-worker
knowledge-worker
outbox-dispatcher
postgres / valkey / qdrant / minio
ollama                  # 可选 profile: local-llm
```

```yaml
# docker-compose.yml 新增
mora-proxy:
  build:
    context: .
    dockerfile: deployments/Dockerfile
  command: ["./mora-proxy"]
  environment:
    - MORA_API_URL=http://mora-api:8080
    - MORA_DB_URL=postgres://mora:mora@postgres:5432/mora
    - MORA_PROXY_LISTEN=:8090
    - MORA_PROXY_AUTH_CACHE_TTL=60s
    - MORA_PROXY_MAX_CONCURRENT=1000
    - MORA_PROXY_STREAM_TIMEOUT=5m
    - MORA_PROXY_INJECT_RATIO=0.2
  depends_on: [mora-api, postgres]
  ports:
    - "8090:8090"
```

### 13.2 Nginx 反向代理

如果 Proxy 前面有 Nginx（现有 `deployments/nginx.conf`），需要确保：

- `/p/` 路径不被缓存。
- SSE 流式响应的 `proxy_buffering off`。
- 路径中的 token 不出现在 access log 中（配置 log format 脱敏）。

```nginx
location /p/ {
    proxy_pass http://mora-proxy:8090;
    proxy_buffering off;           # SSE 流式必须关闭缓冲
    proxy_read_timeout 5m;         # 长对话超时
    proxy_set_header X-Real-IP $remote_addr;
    # Access log 脱敏: 将 /p/{token}/ 替换为 /p/{prefix}.../
}
```

### 13.3 Ollama 集成

Ollama 作为可选 profile：

```yaml
# docker-compose.yml profile: local-llm
ollama:
  image: ollama/ollama:latest
  volumes:
    - ollama-models:/root/.ollama
  ports:
    - "11434:11434"
  profiles: ["local-llm"]
```

Agent 注册时配置：
```text
llm_provider = "ollama"
llm_base_url = "http://ollama:11434"  (Docker 网络)
              或 "http://localhost:11434" (主机网络)
```

Agent SDK 配置：
```text
base_url = http://mora-proxy:8090/p/{proxy_token}/v1
api_key  = ollama                       # 占位值，Ollama 忽略
model    = qwen2.5                       # Ollama 中已拉取的模型
```

### 13.4 水平扩缩容

Proxy 无状态，可水平扩缩容。多实例间共享：
- Token 认证缓存（通过 Valkey 共享，可选；首版每实例独立缓存也可接受）。
- 限流计数器（通过 Valkey 共享，必须；否则多实例下限流失效）。

多实例部署时，Evidence 捕获的提交是幂等的（通过 `Idempotency-Key`），重复提交不会产生重复 Candidate。

---

## 14. 可观测性

### 14.1 指标

| 指标 | 说明 |
|---|---|
| `proxy_requests_total{provider,status}` | 请求总数和结果 |
| `proxy_request_duration_seconds{provider}` | 端到端延迟（Agent -> Proxy -> LLM -> Agent） |
| `proxy_inject_duration_seconds{type}` | 注入延迟（memory / skill / total） |
| `proxy_inject_total{result}` | 注入结果（success / degraded / skipped） |
| `proxy_forward_duration_seconds{provider}` | LLM 转发延迟 |
| `proxy_stream_active_connections` | 活跃流式连接数 |
| `proxy_capture_total{result}` | Evidence 捕获结果 |
| `proxy_capture_duration_seconds` | 捕获提交延迟 |
| `proxy_auth_cache_hit_ratio` | Token 认证缓存命中率 |
| `proxy_rate_limited_total` | 限流拒绝 |
| `proxy_context_tokens{type}` | 上下文预算消耗（memory / skill / agent / total） |

### 14.2 Trace

每个请求的 trace span 链：

```text
proxy.request (root span)
  ├── proxy.auth          (路径 Token 认证)
  ├── proxy.inject         (Memory + Skill 注入)
  │   ├── proxy.inject.context_extract   (上下文提取)
  │   ├── proxy.inject.memory_recall     (Context Broker 调用)
  │   └── proxy.inject.skill_load        (Skill 内容加载)
  ├── proxy.forward        (LLM 请求转发)
  └── proxy.capture        (Evidence 捕获，异步)
```

Trace 中不包含 LLM API Key、完整对话内容和 Memory 正文。

### 14.3 SLO

| 指标 | 目标 |
|---|---|
| Proxy 端到端 P95（不含 LLM 响应） | <= 100ms（认证 + 注入） |
| Memory 注入 P95 | <= 500ms（含 Context Broker 调用） |
| 流式首 event 透传延迟 P95 | <= 50ms |
| Evidence 捕获提交 P95 | <= 200ms（异步，不阻塞响应） |
| LLM API Key 泄露 | 0 |
| 注入降级率 | <= 5%（非 LLM 不可用原因） |
| 被动捕获丢失率 | <= 0.1%（Proxy 重启等） |

---

## 15. 不变量与门禁

### 15.1 不变量

1. LLM API Key 不出现在 PostgreSQL、MinIO、Valkey 或任何持久化存储中。
2. LLM API Key 不出现在审计日志、结构化日志、trace span 或 access log 中。
3. Proxy Token 只存 SHA-256 hash，明文只在创建时返回一次。
4. 被动捕获的 Memory Candidate 和主动提交的走同一条治理管线；被动捕获不绕过 Review。
5. 注入的 Memory 必须是 `state = published` 的；Candidate 和 pending 状态不注入。
6. 注入的 Skill 必须通过静态校验（`validation_status = passed`）；未通过校验的不注入。
7. Proxy 不直接查 PG/Qdrant，所有知识操作通过 Mora API 内部接口。
8. Proxy 不改写 `messages[]` 以外的请求字段（`model`、`temperature`、`tools` 等原样透传）。
9. Proxy 不改写 LLM 响应内容（原样透传）。
10. Agent 的 `llm_provider` 为 NULL 时不转发，返回 409。
11. 路径 Token 撤销后，下一次请求同步拒绝（认证缓存按 revision 失效）。
12. Evidence 捕获提交幂等（`Idempotency-Key`），重复提交不产生重复 Candidate。

### 15.2 测试门禁

| 门禁 | 要求 |
|---|---|
| API Key 隔离 | 全量审计日志、trace、access log 中搜索 LLM API Key pattern，命中 0 |
| Token 安全 | 撤销 token 后下一次请求 401；过期 token 401；伪造 token 401 |
| 注入降级 | Context Broker 超时时请求正常转发；Qdrant 不可用时降级到 FTS |
| 流式透传 | SSE 流逐 event 透传，首 event 延迟 <= 50ms；流结束后 Evidence 捕获触发 |
| 治理一致 | 被动捕获的 Memory Candidate 进入 Review Inbox，不自动发布 |
| 头部适配 | Anthropic provider 正确转换 `Authorization` -> `x-api-key`；Ollama 去掉 `Authorization` |
| 预算控制 | Agent 请求接近 context window 时，Mora 注入自动缩减或跳过 |
| 幂等提交 | 同一 `Idempotency-Key` 重复提交 Evidence，不产生重复 Candidate |
| 并发安全 | 1000 并发请求下 Proxy 无 panic、无内存泄漏 |

---

## 16. 待决问题

1. **Capture mode 的粒度**：`capture_mode` 是按 Agent Binding 配置，还是按 workspace 全局配置，还是两者兼有（workspace 黀 + Agent 覆盖）？
2. **Memory 注入的缓存策略**：相同上下文短时间内重复请求时，是否缓存 Context Broker 检索结果？缓存 key 如何定义（authz revision + query hash + agent binding version）？TTL 多长？
3. **多轮对话的上下文提取策略**：多轮对话中，每轮都重新做 memory recall，还是基于会话 ID 复用上轮结果？Proxy 是否维护会话状态（当前设计无状态）？
4. **Token 预算的精确计算**：近似估算（字符数 * 系数）还是引入 tokenizer 库？不同模型的 tokenizer 不同，引入依赖的代价如何取舍？
5. **Azure OpenAI 的特殊路由**：Azure 的端点格式是 `{endpoint}/openai/deployments/{deployment_id}/chat/completions?api-version=xxx`，Proxy 如何从 Agent 配置中获取 `deployment_id` 和 `api-version`？
6. **Proxy 与 MCP 混用时的 memory 一致性**：Agent 同时通过 Proxy 被动保存和通过 MCP 主动保存时，如何避免重复 Evidence？去重逻辑在提交端还是提炼端？
7. **Provider 健康检查**：Proxy 是否定期探测 LLM provider 可用性？探测时用什么 API Key？还是只在请求失败时标记降级？
8. **Path Token 的 URL 兼容性**：部分 HTTP 中间件可能对长 URL path 做截断或拒绝。Proxy Token 的 base64url 编码长度（约 22 字符）是否足够短，不需要担心中间件兼容性？

---

## 17. 与现有文档的关系

| 现有文档 | 本设计的关系 |
|---|---|
| 11 蓝图 | 放宽"不拦截所有模型请求"的非目标：Proxy 是可选层，不强制拦截 |
| 12 架构 | 放宽"首版不提供透明 Proxy 自动捕获"（§8.1）：提供可选 Proxy，但治理红线不变 |
| 06 MCP | 不影响 MCP Server 设计；Proxy 与 MCP 共存，共享 auth 和 moraclient |
| 03 数据模型 | 扩展 `api_tokens` 和 `agents` 表，新增 `token_purpose`、`llm_provider`、`llm_base_url` |
| 07 安全 | 扩展审计事件和信任边界；LLM API Key 三条红线 |
| 02 系统架构 | 新增 `mora-proxy` 进程到部署拓扑 |
| 05 RAG | 不影响；Memory 注入走 Context Broker，不直连 RAG |

### 17.1 对文档 12 的修订建议

文档 12 §8.1 原文：

> 首版不提供透明 Proxy 自动捕获。任何写入先形成私有 Evidence 和 Candidate，不直接进入团队召回。

建议修订为：

> 首版不强制提供透明 Proxy 自动捕获。Context Proxy 作为可选层，当 Agent 通过路径 Token 接入时被动注入 Memory 和 Skill，并异步捕获 Evidence。被动捕获仍先形成私有 Evidence 和 Candidate，不直接进入团队召回。治理红线不因 Proxy 模式而放松。

文档 11 §1.3 非目标原文：

> 不在第一阶段拦截所有模型请求或接管完整 Agent Runtime。

建议补充：

> Context Proxy 作为可选接入层，不强制拦截所有模型请求。Agent 可选择直连 MCP（主动模式）或通过 Proxy 接入（被动模式），或两者混用。Proxy 不接管 Agent Runtime 的工具调用循环和上下文管理。
