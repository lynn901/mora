# MCP Server 设计

> 文档版本：v1.0 ｜ 产出人：Wiki 知识库架构师 ｜ 对应任务：YS-5
> 依据：PRD §4 模块三（F3.1/F3.2/F3.3）、§5.2 交互流程、§6.3 MCP 域、§9 非功能需求
> 技术选型：Go MCP Server（HTTP/SSE 为主，stdio 为辅），见 01-tech-selection-decision.md

---

## 1. 设计目标

向外部 AI Agent 生态暴露符合 **Model Context Protocol (MCP)** 规范的标准服务端点，实现：
- **协议合规**：标准 initialize/capabilities 握手、tools/resources 列表与调用。
- **能力暴露**：Resources（页面列表/目录树/文档元数据）+ Tools（语义检索/读文档/建草稿/更新文档）。
- **安全复用**：API Key/Bearer Token 鉴权，身份透传复用 Wiki RBAC 引擎，杜绝越权。
- **可观测**：全量审计、限流、连接可观测。

### 1.1 非功能约束（PRD §9）
| 维度 | 要求 |
|---|---|
| 限流 | 按 token 限流，默认 100 req/min，可配 |
| 鉴权 | Token 鉴权拦截率 100%，越权一律 403 + 告警 + 审计 |
| 审计 | 调用全量记录，追加写不可篡改 |
| 安全 | 只读无权限返回空集不报存在（防存在性泄露）；写操作无权限返回 403 |
| 兼容 | MCP 协议版本向后兼容 |
| 部署 | 无状态多副本，K8s HPA |

---

## 2. 传输层设计

### 2.1 传输方式选型

| 传输方式 | 用途 | MVP/Priority | 说明 |
|---|---|---|---|
| **HTTP/SSE** | 远程 Agent（主流） | **MVP（M7）** | JSON-RPC 2.0 over HTTP POST + SSE 推送；面向云端/远程 Agent |
| **stdio** | 本地 Agent（辅助） | P2（C1） | JSON-RPC 2.0 over stdin/stdout；面向本地 CLI Agent |

**决策：HTTP/SSE 为主，stdio 为辅。** PRD F3.1 要求二选一可配；MVP 优先 HTTP/SSE 覆盖远程 Agent 主场景，stdio 作为 P2 扩展。

### 2.2 HTTP/SSE 传输实现

遵循 MCP 规范的 Streamable HTTP 传输：

```
Agent                          MCP Server
  │                                │
  │── POST /mcp (JSON-RPC initialize, Accept: application/json, text/event-stream)
  │◄── 200 (SSE 流 / JSON 响应, 含 Mcp-Session-Id 头)
  │                                │
  │── POST /mcp (tools/list, 带 Mcp-Session-Id)
  │◄── 200 (JSON-RPC 响应)
  │                                │
  │── POST /mcp (tools/call search_knowledge_base)
  │◄── 200 (结果)
  │                                │
  │── DELETE /mcp (Mcp-Session-Id)  关闭会话
  │◄── 200
```

- **端点**：`POST /mcp`（统一 JSON-RPC 入口）；可选 `GET /mcp`（SSE 长连接，服务端推送）。
- **会话**：`initialize` 时返回 `Mcp-Session-Id`，后续请求携带以维持会话状态。
- **SSE**：服务端可通过 SSE 流推送进度/通知（如长检索任务的增量结果）。
- **鉴权**：HTTP 头 `Authorization: Bearer <api_token>`，每个请求校验。
- **CORS**：若 Agent 为浏览器侧，配置允许的 Origin 白名单。

### 2.3 stdio 传输实现（P2）

```
Agent 进程 ──stdin──▶ MCP Server 子进程 ──stdout──▶ Agent
每行一个 JSON-RPC 2.0 消息（newline-delimited JSON）。
```
- 本地启动 `mcp-server --transport stdio`，Agent 作为父进程拉起。
- 鉴权：启动参数传入 `--api-token` 或环境变量，会话级固定身份。
- 适用：本地 CLI Agent、IDE 集成。

### 2.4 协议版本兼容

- `initialize` 响应声明 `protocolVersion`（如 `2025-06-18`），并声明 `capabilities`。
- 向后兼容：支持旧版协议版本，对未知方法返回标准 JSON-RPC error `-32601 method not found`。
- 非 MCP 规范请求返回 HTTP 400 + JSON-RPC error。

---

## 3. MCP 协议交互

### 3.1 initialize 握手

```jsonc
// Agent → Server
{
  "jsonrpc": "2.0", "id": 1, "method": "initialize",
  "params": {
    "protocolVersion": "2025-06-18",
    "capabilities": { "roots": { "listChanged": true } },
    "clientInfo": { "name": "my-agent", "version": "1.0.0" }
  }
}

// Server → Agent
{
  "jsonrpc": "2.0", "id": 1,
  "result": {
    "protocolVersion": "2025-06-18",
    "capabilities": {
      "tools": { "listChanged": true },
      "resources": { "list": true, "read": true, "listChanged": true }
    },
    "serverInfo": { "name": "wiki-mcp", "version": "1.0.0" }
  }
}
```
随后 Agent 发送 `notifications/initialized` 完成握手。

### 3.2 能力声明

| 能力 | 支持 | 说明 |
|---|---|---|
| `tools` | ✅ list / call / listChanged | 工具调用 |
| `resources` | ✅ list / read / listChanged | 资源读取（URI scheme） |
| `prompts` | ❌（本期不暴露） | — |
| `logging` | ✅ | 服务端可向 Agent 推送日志 |
| `completion` | ❌ | — |

---

## 4. Resources 清单

Resources 以 URI 暴露只读知识结构，供 Agent 感知 Wiki 结构（PRD F3.2）。

| URI | 方法 | 描述 | RBAC |
|---|---|---|---|
| `wiki://workspaces` | `resources/list`/`read` | 当前 Token 身份可见的工作区列表 | 仅返回有 read 权限的工作区 |
| `wiki://workspaces/{ws_id}/tree` | `resources/read` | 工作区目录树（含文档标题/ID，不含正文） | 仅返回可见目录/文档 |
| `wiki://documents/{doc_id}/meta` | `resources/read` | 文档元数据（标题/状态/版本/标签/索引状态） | 无 read 权限返回空/不存在 |
| `wiki://workspaces/{ws_id}/tags` | `resources/read` | 工作区标签体系 | 工作区可见即可读 |
| `wiki://documents/{doc_id}/versions` | `resources/read` | 文档版本历史摘要 | 有 read 权限 |

### 4.1 resources/list 示例

```jsonc
// Agent → Server
{ "jsonrpc": "2.0", "id": 2, "method": "resources/list",
  "params": { "cursor": null } }

// Server → Agent
{
  "jsonrpc": "2.0", "id": 2,
  "result": {
    "resources": [
      { "uri": "wiki://workspaces", "name": "可见工作区", "mimeType": "application/json" },
      { "uri": "wiki://workspaces/ws-1/tree", "name": "工程团队-目录树", "mimeType": "application/json" },
      { "uri": "wiki://documents/doc-1/meta", "name": "API设计规范-元数据", "mimeType": "application/json" }
    ],
    "nextCursor": null
  }
}
```

### 4.2 resources/read 示例

```jsonc
// Agent → Server
{ "jsonrpc": "2.0", "id": 3, "method": "resources/read",
  "params": { "uri": "wiki://documents/doc-1/meta" } }

// Server → Agent
{
  "jsonrpc": "2.0", "id": 3,
  "result": {
    "contents": [{
      "uri": "wiki://documents/doc-1/meta",
      "mimeType": "application/json",
      "text": "{\"id\":\"doc-1\",\"title\":\"API设计规范\",\"status\":\"published\",\"version_no\":5,\"tags\":[\"api\"],\"index_status\":\"indexed\"}"
    }]
  }
}
```

---

## 5. Tools 清单

Tools 暴露可执行操作（PRD F3.2）。

### 5.1 工具总表

| Tool | 类型 | 描述 | 鉴权/RBAC | MVP |
|---|---|---|---|---|
| `search_knowledge_base` | 只读 | 语义混合检索（Dense+BM25+Rerank） | read 权限过滤 | ✅ M7 |
| `get_document` | 只读 | 读取文档正文 | read 权限；无权返回空不报存在 | ✅ M7 |
| `list_documents` | 只读 | 列出工作区/目录下文档 | read 权限过滤 | ✅ |
| `get_tags` | 只读 | 获取标签体系 | 工作区可见 | ✅ |
| `create_draft` | 写 | 创建草稿（不直接发布） | write 权限；进草稿/审阅态 | P1 S3 |
| `update_document` | 写 | 更新文档内容（产生新版本） | write 权限；进草稿/审阅态 | P1 S3 |

### 5.2 工具 Schema 定义

#### 5.2.1 search_knowledge_base
```jsonc
{
  "name": "search_knowledge_base",
  "description": "在 Wiki 知识库中进行语义混合检索（稠密向量+BM25+重排），结果严格遵循调用方权限。",
  "inputSchema": {
    "type": "object",
    "required": ["query"],
    "properties": {
      "query": { "type": "string", "description": "自然语言查询" },
      "workspace_id": { "type": "string", "description": "限定工作区（可选）" },
      "directory_id": { "type": "string", "description": "限定目录（可选）" },
      "tags": { "type": "array", "items": { "type": "string" } },
      "top_k": { "type": "integer", "default": 50 },
      "top_n": { "type": "integer", "default": 10 },
      "rerank": { "type": "boolean", "default": false, "description": "启用重排（P1）" }
    }
  }
}
```
**返回**：命中文档列表（document_id/title/chunk_text/score/source_url）。无权限命中不返回、不计入 total。

#### 5.2.2 get_document
```jsonc
{
  "name": "get_document",
  "description": "读取文档正文（Block JSON 或 Markdown）。无权限时返回空结果而非错误，防止存在性泄露。",
  "inputSchema": {
    "type": "object",
    "required": ["document_id"],
    "properties": {
      "document_id": { "type": "string" },
      "format": { "type": "string", "enum": ["blocks", "markdown"], "default": "markdown" },
      "version_no": { "type": "integer", "description": "指定版本（可选，默认最新）" }
    }
  }
}
```
**返回**：文档正文。无权限 → 返回空 content（非 403，防存在性推断）。

#### 5.2.3 create_draft（P1）
```jsonc
{
  "name": "create_draft",
  "description": "创建文档草稿，不直接发布。需人工/流程审阅后发布并触发向量化。",
  "inputSchema": {
    "type": "object",
    "required": ["workspace_id", "title", "content"],
    "properties": {
      "workspace_id": { "type": "string" },
      "parent_id": { "type": "string", "description": "父目录（可选）" },
      "title": { "type": "string" },
      "content": { "type": "string", "description": "Markdown 或 Block JSON" },
      "format": { "type": "string", "enum": ["blocks", "markdown"], "default": "markdown" }
    }
  }
}
```
**返回**：草稿 ID + 审阅链接。无 write 权限 → 403。

#### 5.2.4 update_document（P1）
```jsonc
{
  "name": "update_document",
  "description": "更新文档内容，产生新版本草稿，待审阅发布。",
  "inputSchema": {
    "type": "object",
    "required": ["document_id", "content"],
    "properties": {
      "document_id": { "type": "string" },
      "content": { "type": "string" },
      "format": { "type": "string", "enum": ["blocks", "markdown"], "default": "markdown" },
      "summary": { "type": "string", "description": "变更摘要" }
    }
  }
}
```
**返回**：新版本草稿 ID + 审阅链接。无 write 权限 → 403。

### 5.3 tools/list 示例

```jsonc
{ "jsonrpc": "2.0", "id": 4, "method": "tools/list" }
// → result.tools = [search_knowledge_base, get_document, list_documents, get_tags, ...]
```

### 5.4 tools/call 示例

```jsonc
// Agent → Server
{ "jsonrpc": "2.0", "id": 5, "method": "tools/call",
  "params": { "name": "search_knowledge_base",
              "arguments": { "query": "如何设计 RESTful API 分页", "top_n": 5 } } }

// Server → Agent
{
  "jsonrpc": "2.0", "id": 5,
  "result": {
    "content": [{
      "type": "text",
      "text": "[{\"document_id\":\"doc-1\",\"title\":\"API设计规范\",\"chunk_text\":\"分页采用 page/page_size...\",\"score\":0.92,\"source_url\":\"/workspaces/ws-1/documents/doc-1\"}]"
    }],
    "isError": false
  }
}
```

---

## 6. 鉴权与 RBAC 复用

### 6.1 身份模型

```
ApiToken (token_hash) ──1:1── Identity (user | service_account) ──继承── RBAC 权限
```
- Token 创建时绑定一个身份（user 或 service_account），继承其 RBAC 权限（见 03-data-model.md §2.8）。
- Token 可配置 `scope`（readonly/readwrite/admin）与 `expires_at`，可即时吊销（`revoked_at`）。

### 6.2 鉴权流程

```
Agent 请求 (Authorization: Bearer <token>)
    │
    ▼
MCP Server 中间件:
  1. 提取 token → SHA256(token) → 查 api_tokens (WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()))
  2. 未找到/已吊销/过期 → 401
  3. 加载 identity (user/service_account) → 构造 AuthContext{identity_id, groups[], scope}
  4. 注入 context，透传到 Tool/Resource 处理器
    │
    ▼
Tool/Resource 处理器:
  - 调用 Wiki RBAC 引擎 (REST /permissions/check 或内部 SDK)
  - 只读工具: RBAC 过滤可见范围 → 无权命中返回空（不报存在）
  - 写工具: 校验 write 权限 → 无权返回 403
```

### 6.3 RBAC 复用

- **不重复实现权限逻辑**：MCP Server 通过内部 HTTP 调用 Wiki API（`INTERNAL_SERVICE_TOKEN`），复用同一 RBAC 引擎与 `/rag/search` 检索服务。
- **检索工具**：调用 `/rag/search` 时携带 identity 上下文，检索服务在 Qdrant payload 层 + PG SQL 层双重 RBAC 过滤（见 05-rag-pipeline-design.md §4.3、§6.3）。
- **写工具**：`create_draft`/`update_document` 调 Wiki API 创建草稿，Wiki 层校验 write 权限。
- **作用域约束**：Token `scope=readonly` 时，写工具直接拒绝（不调用 Wiki 层）。

### 6.4 防存在性泄露
- 只读工具（`get_document`/`search_knowledge_base`/Resources）无权限时返回**空结果**，不返回 403/404，防止 Agent 推断文档存在性（PRD F3.2 边界）。
- 写工具无权限返回 **403**（写操作不存在"不可推断"诉求）。

---

## 7. 审计与限流

### 7.1 审计（全量记录）

每次 Tool/Resource 调用记录到 `mcp_tool_calls` + `audit_logs`（见 03-data-model.md §2.8、§2.6）：

| 字段 | 内容 |
|---|---|
| session_id | MCP 会话 ID |
| token_id / identity | 调用方身份 |
| tool_name | 工具/资源名 |
| params_summary | 参数摘要（脱敏：截断长文本、剔除敏感字段） |
| result_status | success / forbidden / error |
| target_resource | 操作目标（document_id 等） |
| duration_ms | 耗时 |
| audit_log_id | 关联审计日志（追加写，不可篡改） |

- **追加写**：`audit_logs` 按月分区，仅 INSERT 不 UPDATE/DELETE（应用层 + PG 权限约束）。
- 越权调用（forbidden）额外触发告警（Prometheus `mcp_forbidden_total`）。
- 管理后台可通过 `/mcp/tool-calls`（见 04-api-contract.md §10）查询审计。

### 7.2 限流

- **维度**：按 `token_id` 限流（滑动窗口，Valkey 实现）。
- **默认**：100 req/min（可配，PRD §9）。
- **分级**：
  - 只读工具：100 req/min
  - 写工具：20 req/min（更严格，防滥用）
- **超限**：返回 JSON-RPC error 或 HTTP 429 + `Retry-After`。
- **实现**：Valkey 滑动窗口计数器（`INCR` + `EXPIRE`），多副本 MCP Server 共享限流状态。

### 7.3 会话管理

- `initialize` 创建 `mcp_sessions` 记录（token_id, transport, client_info, capabilities, started_at）。
- 会话关闭（`DELETE /mcp` 或超时）写 `ended_at`。
- Token 吊销后，关联活跃会话立即失效（中间件校验 token 状态）。

---

## 8. 模块结构（Go）

```
internal/module/mcp/
├── server/          # MCP 协议实现（JSON-RPC 路由、握手、会话）
│   ├── server.go    # MCP Server 主体
│   ├── transport_http.go   # HTTP/SSE 传输
│   ├── transport_stdio.go  # stdio 传输（P2）
│   └── session.go   # 会话管理
├── resource/        # Resources 实现（URI → 处理器）
│   └── resource.go
├── tool/            # Tools 实现（name → 处理器，调 Wiki/RAG REST）
│   ├── search.go    # search_knowledge_base
│   ├── document.go  # get_document / create_draft / update_document
│   └── tool.go      # 注册中心
├── auth/            # Token 鉴权 + RBAC 透传 + 限流
│   ├── middleware.go
│   ├── token.go
│   └── ratelimit.go
└── audit/           # 审计记录
    └── audit.go

cmd/mcp-server/main.go   # 入口，按 --transport 启动 HTTP/stdio
```

- MCP Server 与 Wiki API 同一代码仓库（模块化单体），共享 `platform/rbac`、`domain`、`pkg`。
- 通过内部 REST 调用 Wiki/RAG 能力，不重复实现业务逻辑（见 02-system-architecture.md §2.2）。

---

## 9. 与 PRD 一致性对照

| PRD 要求 | 本设计 | 状态 |
|---|---|---|
| F3.1 MCP 协议合规（initialize/capabilities） | §3 协议交互 | ✅ |
| F3.1 HTTP/SSE 为主，stdio 为辅 | §2 传输层 | ✅ |
| F3.1 协议版本兼容 | §2.4 | ✅ |
| F3.2 Resources（工作区列表/目录树/文档元数据） | §4 Resources | ✅ |
| F3.2 Tools（search/get_document/create_draft/update_document） | §5 Tools | ✅ |
| F3.2 写操作默认进草稿/审阅态 | §5.2.3/5.2.4 | ✅ |
| F3.3 API Key/Bearer Token 鉴权 + 透传身份 | §6 鉴权 | ✅ |
| F3.3 RBAC 复用，越权 403 | §6.3/6.4 | ✅ |
| F3.3 审计全量记录不可篡改 | §7.1 | ✅ |
| F3.3 限流按 token | §7.2 | ✅ |
| §7 边界: 只读无权返回空集不报存在；写无权 403 | §6.4 | ✅ |
| §7 边界: Token 吊销会话即时失效 | §7.3 | ✅ |
| AC-14~AC-19 | 全覆盖 | ✅ |

---

> 本设计为 Stage 1 门禁交付物（交付物 #6）。与 04-api-contract.md（§10 MCP 域）、03-data-model.md（§2.8 MCP 表）共同构成 MCP 模块完整契约，Stage 2 YS-9 可据此并行研发。
