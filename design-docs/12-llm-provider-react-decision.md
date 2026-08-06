# LLM Provider + ReAct 技术选型决策书

> 文档版本：v1.0（Provider 章节定稿，ReAct 章节草案挂起待 Provider 定后补入）｜ 产出人：Mora项目架构师 ｜ 对应任务：YS-70《分析 WeKnora 功能并制定 Mora 整体规划》§1.3 / 阶段二·三 P1→P2
> 父决策：01-tech-selection-decision.md、05-rag-pipeline-design.md §5（Embedding Provider 抽象）、06-mcp-server-design.md §5（MCP 工具层）
> 产品基线：YS-70 PM 初版规划 §1.3「ReAct Agent」（重新定义为知识库内封闭 Agent，单轮检索-起草走 create_draft 审阅态，多步 ReAct 列 P1/P2，不照搬外部问答）
> 评审状态：**§1–§5（LLM Provider）定稿；§6–§8（ReAct）草案挂起**——Provider 抽象先收敛，ReAct 实现待 Provider 定 + draft/review 后端门控解除后补入

---

## 0. 结构说明（响应 PM 两点硬要求）

PM 两点硬要求：① draft/review 后端门控显式标注（§7）；② LLM Provider 抽象先于 ReAct 定稿。本决策书据此分两块：

- **§1–§5：LLM Provider（定稿）**——接口抽象、选型、配置、与现有 `EmbeddingProvider` 对齐。可独立交付研发实现。
- **§6–§8：ReAct（草案挂起）**——自研循环设计、封闭工具集约束、draft/review 门控与立项建议。ReAct 实现依赖 Provider 接口先定（§3）+ draft/review 后端门控解除（§7），故 ReAct 章节挂起，待两前置满足后补入完整实现细节，**避免循环实现先于抽象**。

---

## 1. 决策背景与范围

### 1.1 背景

Mora 现状（已核实）：`internal/module/rag/provider/provider.go` 有 `EmbeddingProvider`/`RerankerProvider` 抽象，`internal/infra/ragwiring/wiring.go` 有 `DefaultProviderFactory.For`（switch `tei`/`ollama`），TEI 主 + Ollama 备。但**无 LLM Provider**（无对话/推理模型接入），`internal/` 全树无 `LLMProvider`/`react`/`chatcompletion` 任何脚手架。PM §1.3 把 Agent 定位为「知识库内封闭 Agent」，单轮 `search`+`get_document`→`create_draft`，多步 ReAct 列 P1/P2。

### 1.2 P1 范围

- **LLM Provider 抽象**（本决策书定稿）：新增 `LLMProvider` 接口，Ollama 本地优先（复用 `OllamaURL`），可选 OpenAI 兼容端点；与现有 `EmbeddingProvider` 对齐范式。
- **Agent 单轮起草**（依赖 Provider + draft/review 门控）：`search`+`get_document`→`create_draft`（走审阅态，不直接发布）。
- **多步 ReAct**（P1→P2）：封闭工具集（仅 Mora 内部 MCP 工具），不开放 web/外部 MCP。

### 1.3 决策原则（继承 01 §1.2）

私有化优先（本地 Ollama 不出网）、License 合规、复用既有抽象范式、可插拔、安全优先（Agent 写入必走审阅，不绕过 RBAC 与版本）。

---

## 2. 候选方案评估（LLM Provider）

### 2.1 实现方式

| 候选 | License / 接入 | 选型定位 |
|---|---|---|
| **`OllamaLLMProvider`**（自研，HTTP 直调 Ollama `/api/chat`） | 自研，复用现有 `OllamaURL`（`config.go`） | **选用**：本地优先，不出网 |
| **`OpenAICompatProvider`**（自研，OpenAI 兼容 `/v1/chat/completions`） | 自研 | **可选**：接企业内部 vLLM/LiteLLM 等兼容端点 |
| 三方 SDK（`sashabaranov/go-openai` 等） | MIT | **不引入**：OpenAI 协议简单，直调 HTTP 即可，避免多余依赖 |
| `langchaingo`（ReAct 框架候选，见 §6） | MIT | 仅 ReAct 层评估，Provider 层不引入 |

**结论**：Provider 层自研 HTTP 客户端，镜像现有 `OllamaProvider`（`provider/ollama.go` 直调 `/api/embeddings`）的范式，调 `/api/chat`。不引入三方 SDK，License 全合规。

### 2.2 选型理由

1. **范式一致**：现有 `OllamaProvider.Embed` 直调 Ollama HTTP，`OllamaLLMProvider.Chat` 同样直调 `/api/chat`，实现风格统一，团队维护成本最低。
2. **本地不出网**：复用 `OllamaURL`（默认 `http://localhost:11434`），本地推理，符合「默认不出网」。
3. **可插拔**：Provider 接口化，`OllamaLLMProvider` + `OpenAICompatProvider` 双实现，配置切换，对标 `EmbeddingProvider` 的 TEI/Ollama 双实现。

---

## 3. 决策结论：LLMProvider 接口（定稿）

### 3.1 接口设计（镜像 `EmbeddingProvider`，`provider.go:13`）

```go
// LLMProvider turns chat messages into a completion. Mirrors EmbeddingProvider
// (05 §5): Ollama local-first, OpenAI-compatible endpoint optional.
type LLMProvider interface {
    // Chat produces a completion for the message history. toolSchemas is the
    // MCP tool definitions the LLM may call (nil = no tools / plain completion).
    // Streaming is optional (P2); P1 returns a single ChatResult.
    Chat(ctx context.Context, req ChatRequest) (*ChatResult, error)
    HealthCheck(ctx context.Context) error
    Name() string
}

type ChatRequest struct {
    Messages     []ChatMessage     // system/user/assistant/tool-result history
    ToolSchemas  []ToolSchema      // MCP tool defs the LLM may invoke (ReAct)
    Temperature  float64
    MaxTokens    int
}

type ChatMessage struct {
    Role    string         // system/user/assistant/tool
    Content string
    ToolCalls []ToolCall    // assistant turn: requested tool invocations
    ToolCallID string       // tool role: the call this responds to
}

type ChatResult struct {
    Content   string         // assistant text
    ToolCalls []ToolCall     // tools the LLM wants invoked (ReAct loop)
    Usage     TokenUsage     // for OTel LLM span (#4 traceability)
}

type ToolCall struct {
    ID    string
    Name  string
    Input json.RawMessage
}

type TokenUsage struct { Prompt, Completion, Total int }
```

落地 `internal/module/rag/provider/llm.go`（与 `ollama.go`/`tei.go` 同包），或新 `internal/module/llm/provider.go`——**建议新 `internal/module/llm`**，因 LLM 服务对象含 Agent（mcp 内），不止 RAG，独立模块边界更清。

### 3.2 Provider 工厂（镜像 `DefaultProviderFactory`，`ragwiring/wiring.go:18`）

```go
// LLMProviderFactory builds the configured LLM provider. Mirrors
// DefaultProviderFactory.For (TEI/Ollama switch).
type LLMProviderFactory interface {
    For(ctx context.Context) (LLMProvider, error)
}

type DefaultLLMProviderFactory struct {
    OllamaURL  string         // 复用现有 OllamaURL
    LLMProvider string        // "ollama" (default) | "openai_compat"
    LLMModel   string
    OpenAICompatURL string    // 可选，OpenAI 兼容端点
}
```

---

## 4. 配置项核实（响应 PM「`LLMURL`/`LLMModel`」之问）

### 4.1 现状（`internal/platform/config/config.go`）

- 现有 `OllamaURL`（默认 `http://localhost:11434`，`config.go:79`）——**Embedding 与 LLM 共用同一 Ollama 实例**。
- 现有 `EmbeddingProvider`/`EmbeddingModel`/`EmbeddingDim`（`config.go:33-35`，env `EMBEDDING_PROVIDER`/`EMBEDDING_MODEL`/`EMBEDDING_DIM`）。

### 4.2 决策：新增 `LLM_*` 配置项，对齐 `EMBEDDING_*` 命名

| 新增字段 | Env | 默认 | 说明 |
|---|---|---|---|
| `LLMProvider` | `LLM_PROVIDER` | `"ollama"` | `ollama` / `openai_compat` |
| `LLMModel` | `LLM_MODEL` | `"qwen2.5:7b"`（可调） | 本地对话模型 |
| `LLMURL` | `LLM_URL` | `""`（空=复用 `OllamaURL`） | **复用 `OllamaURL` 时不新增端点**；OpenAI 兼容模式填此 |
| `LLMTemperature` | `LLM_TEMPERATURE` | `0.2` | Agent 偏低温度 |
| `LLMMaxTokens` | `LLM_MAX_TOKENS` | `2048` | 单轮上限 |

**回答 PM 之问**：

- **`OllamaLLMProvider` 是否需新增 `LLM_URL`/`LLM_MODEL`？需要。** 理由：① 对齐 `EMBEDDING_*` 命名一致性（现有范式）；② LLM 模型名与 Embedding 模型名不同（Embedding 用 `all-MiniLM-L6-v2`，LLM 用对话模型），**不能复用 `EmbeddingModel` 字段**；③ `LLM_URL` 默认空时复用 `OllamaURL`（同一 Ollama 实例出 Embedding + LLM），但允许 OpenAI 兼容模式独立配置端点——这是「本地优先 + 可选外部兼容」的关键开关。
- **是否复用 `OllamaURL`？是，默认复用。** `LLM_URL` 留空 → `OllamaLLMProvider` 用 `OllamaURL`，不新增端点配置负担；仅当切 OpenAI 兼容端点时填 `LLM_URL`。

### 4.3 被低估改动核实

- ✅ **无低估改动**：`OllamaURL` 复用是零成本（同一 Ollama 实例既出 Embedding 又出 LLM 是 Ollama 常规用法）；新增 `LLM_*` 配置项是纯增量，对齐现有 `EMBEDDING_*` 范式，不改现有字段。
- ⚠️ **一处提示**：`EmbeddingDim`（384）是向量维度，LLM 无对应概念，**勿误加 `LLMDim`**——`LLMMaxTokens` 才是 LLM 的「容量」参数。已在 §4.2 表中正确区分。
- ⚠️ **cmd 绑定点**：`cmd/rag-worker/main.go`（或 `cmd/mora-api`）需注入 `LLMProviderFactory`，对齐 `DefaultProviderFactory` 在 rag-worker 的注入位置。属实现项，非架构改动。

---

## 5. License 合规声明

| 组件 | License | 合规影响 |
|---|---|---|
| `OllamaLLMProvider`（自研 HTTP） | 自研 | 无传染 |
| `OpenAICompatProvider`（自研 HTTP） | 自研 | 无传染 |
| Ollama（运行时） | MIT | 本地推理，无传染 |

**合规结论**：Provider 层自研，无新增依赖，License 全合规。

---

## 6. ReAct 实现方式（草案，挂起待 Provider 定 + 门控解除）

> 本节为草案。ReAct 实现依赖：① §3 `LLMProvider` 接口定稿（已定，本决策书）；② §7 draft/review 后端门控解除（未解）。ReAct 完整实现细节待两前置满足后补入。

### 6.1 自研轻量循环 vs 框架：推荐自研

ReAct 本质循环：system prompt + 工具 schema → `LLMProvider.Chat` → 解析 `ToolCalls` → 执行已注册 MCP 工具 → 结果回灌 → 重复至无调用或达上限。Mora 已有 `server.ToolHandler`（`mcp/server/server.go:19`）+ `ToolDef`（各工具 `Definition()`）提供工具 schema 面，agent 枚举注册工具即构造 LLM 工具集，**约 200 行循环可闭合 MVP**。`langchaingo`(MIT) 虽轻但拉入大面积依赖、且其抽象对「封闭工具集、无 web」的 Mora 场景过度。**不引框架。**

### 6.2 封闭工具集约束落地

结构性收敛：agent 工具注册表 = 仅已注册 MCP 工具（`search_knowledge_base`/`list_documents`/`get_document`/`get_tags`/`create_draft`/`update_document`）。循环只认识注册处理器，**无 web 搜索、无远程 MCP、无沙箱**——与 PM「不照搬外部问答 ReAct」一致。写操作走 `create_draft` 审阅态（依赖 §7 门控解除）。

### 6.3 单轮起草（P1）vs 多步 ReAct（P1→P2）

- **P1 单轮**：`search`+`get_document`→`create_draft`（一次 LLM 调用 + 一次工具写）。无循环，最简。
- **P2 多步**：循环 + 并行工具调用 + 人工审批节点（敏感写操作）。列 P2。

### 6.4 挂起原因（重申）

ReAct 实现细节（循环状态机、上下文截断、工具并行调度、超时与重试、human-in-the-loop 审批 UI）待 §3 Provider 定稿（已定）+ §7 draft/review 门控解除后补入。Provider 接口已含 `ToolSchemas`/`ToolCalls`/`TokenUsage`，ReAct 循环可据此构造，**抽象已先行**。

---

## 7. draft/review 后端门控（显式标注 + 立项建议）

### 7.1 门控现状（已核实，代码依据）

Agent 单轮起草的写入路径**部分存在但生命周期未闭合**：

- ✅ **MCP 工具层已实现**：`mcp/tool/document.go:59-89` 的 `CreateDraftTool`/`UpdateDocumentTool`（`IsWrite()=true`，scope 网关前置）。
- ✅ **MoraClient 接口已定义**：`moraclient/client.go:164-190` 的 `CreateDraft`/`UpdateDocument`。
- ✅ **HTTPClient 实现已调真实端点**：`moraclient/http.go:314` `CreateDraft` 调 `POST /api/v1/workspaces/{ws}/documents`（`status=draft`），`UpdateDocument` 调 `PATCH /api/v1/documents/{id}`——**这两个端点存在**（`cmd/mora-api/main.go:152,154` 注册了 `docH.Create`/`docH.Update`）。
- ❌ **`ReviewURL` 是伪造路径**：`http.go:325` 返回 `ReviewURL: "/review/"+doc.ID`，但 `cmd/mora-api/main.go:151-159` **未注册 `/review/:id` 路由**——该 URL 无后端。
- ❌ **无审阅生命周期**：`domain/user.go:64-67` 的 `DocumentStatus` 仅 `draft`/`published`/`archived`/`deleted`，**无 `in_review`/`approved`/`rejected` 等审阅状态**；`documents` 表 `status` 列（`003:13`）同此枚举。
- ❌ **无审阅 handler/路由/状态机**：无 `draft→in_review→published/rejected` 转移端点，无审阅列表，无审批人指派。

**门控结论**：Agent 能**创建 draft 文档**（复用 Create handler，`status=draft`），但**无法走完整审阅闭环**（无 `/review/:id`、无状态转移、无审批）。`ReviewURL` 是死链。这是 P1 Agent 单轮起草闭环的关键前置。

### 7.2 立项建议：draft/review 后端**单独立项**，不并入本决策书

| 维度 | 并入本决策书 | **单独立项（推荐）** |
|---|---|---|
| 范围 | LLM Provider + ReAct + draft/review 后端 | LLM Provider + ReAct 在此；draft/review 后端另立 |
| 涉及面 | 域模型（`DocumentStatus` 扩状态）+ 迁移（`documents` 加审阅字段/新表）+ handler/路由 + service 状态机 + 审阅列表/指派 UI + MCP `ReviewURL` 落地 | 本决策书聚焦 LLM+ReAct；后端由 mora 域研发承接 |
| 评审复杂度 | 三域耦合，评审重 | 解耦，各自评审 |
| 依赖 | ReAct 依赖 draft/review，耦合 | ReAct 挂起待 draft/review，但 Provider 不依赖，可先行 |

**推荐单独立项**，理由：

1. **域归属不同**：draft/review 是 mora 文档域的核心生命周期扩展（动 `DocumentStatus` 枚举 + `documents` 表 + `document_versions` 审阅关联），属 mora 域研发，非 LLM/Agent 域；本决策书是 LLM+ReAct 架构，跨域并入会模糊归属。
2. **可解耦推进**：LLM Provider（§1–§5）**不依赖** draft/review，可立即交付研发实现；ReAct（§6）挂起待门控；draft/review 后端独立排期后，ReAct 即可补入。三路并行，Provider 先行不阻塞。
3. **门控显式化**：单独立项使门控成为可见的 P1 前置 issue，排期透明，避免「ReAct 做到一半发现 write 路径没闭环」。

**建议落地**：由 PM 起一个 P1 issue「draft/review 文档审阅后端」（域模型扩 `in_review`/`approved`/`rejected` 状态 + `documents` 加 `reviewer_id`/`reviewed_at` 或新 `document_reviews` 表 + `/review/:id` 与状态转移路由 + MCP `ReviewURL` 落地 + 审阅列表/指派），本决策书 §6 ReAct 在该 issue 闭环后补入完整实现。

---

## 8. 门控与被低估改动汇总

| 项 | 状态 | 说明 |
|---|---|---|
| LLM Provider 抽象（§3） | ✅ 定稿 | 镜像 `EmbeddingProvider`，`OllamaLLMProvider`+`OpenAICompatProvider` |
| `LLM_*` 配置项（§4） | ✅ 定稿 | 新增，对齐 `EMBEDDING_*`，`LLM_URL` 空时复用 `OllamaURL`，无低估改动 |
| ReAct 自研循环（§6） | 🔶 草案挂起 | 待 Provider 定（已定）+ draft/review 门控解除 |
| **draft/review 后端门控（§7）** | ⚠️ **外部门控，未解** | `ReviewURL` 死链 + 无审阅状态机；建议单独立项，不并入本决策书 |
| cmd Provider 工厂注入 | 实现项 | 对齐 `DefaultProviderFactory` 注入位置，非架构改动 |
| 三方 SDK | 零 | Provider 直调 HTTP，ReAct 自研，不引 langchaingo/go-openai |

**结论**：LLM Provider（§1–§5）路径最短、无外部门控，可立即交付研发；ReAct（§6）挂起待 draft/review 后端（§7，建议单独立项）闭环后补入。Provider 抽象先行满足 PM「抽象先于实现」要求。

### 8.1 估期（架构层粗估，供 PM 排期，研发定稿为准）

| 项 | 估期 | 说明 |
|---|---|---|
| `LLMProvider` 接口 + `OllamaLLMProvider` + 测试 | 2–3d | 镜像 `OllamaProvider`，调 `/api/chat` |
| `OpenAICompatProvider` + 测试 | 1–2d | OpenAI 兼容端点 |
| `LLM_*` 配置项 + 工厂注入 | 1d | 对齐 `EMBEDDING_*` |
| LLM 调用 OTel span（衔接 #4） | 0.5d | 预留 #4 追踪接入点 |
| Provider 小计 | **~5d** | 可立即启动，不依赖门控 |
| ReAct 自研循环 + 测试 | 3–4d | 挂起，待 draft/review 闭环 |
| 单轮起草 agent 接线 | 1–2d | 挂起 |
| ReAct 小计 | **~5d** | 待门控解除 |

---

> 本决策书 §1–§5（LLM Provider）定稿，§6–§8（ReAct）草案挂起。Provider 抽象先于 ReAct 收敛，满足 PM 硬要求②；draft/review 后端门控在 §7 显式标注并建议单独立项，满足 PM 硬要求①。研发可依 §1–§5 立即实现 `LLMProvider`/`OllamaLLMProvider`/`OpenAICompatProvider` 与 `LLM_*` 配置；ReAct 待 draft/review 后端 issue 闭环后依 §6 补入。
