# 数据模型设计

> 文档版本：v1.0 ｜ 产出人：Mora 知识库架构师 ｜ 对应任务：YS-5
> 依据：PRD §6 实体关系 ｜ 技术选型：PostgreSQL 16+ + Qdrant 1.8+

---

## 1. ER 概览

基于 PRD §6 实体关系，落地为 PostgreSQL schema。以下为关键实体关系图（与 PRD §6.4 一致）。

```
Workspace 1──N Directory N──N(tree) Document 1──N DocumentVersion
Document 1──N Block / 1──N Attachment / N──N Tag
Document 1──N IndexingTask 1──N Chunk (Qdrant)
Permission N──1 Role, target → Workspace/Directory/Document
ApiToken 1──1 Identity(User/ServiceAccount)
McpSession N──1 ApiToken, McpSession 1──N McpToolCall
```

---

## 2. PostgreSQL Schema（迁移脚本）

> 以下为 SQL DDL，对应 `migrations/` 目录下的迁移文件。采用 `up/down` 双向迁移。
> 主键统一为 `UUID`（gen_random_uuid()），分布式友好。

### 2.1 迁移 001：用户与身份

```sql
-- migrations/001_users.up.sql

CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "zhparser";      -- 中文分词
CREATE EXTENSION IF NOT EXISTS "ltree";          -- 目录树路径（可选）

-- 用户
CREATE TABLE users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       VARCHAR(255) NOT NULL UNIQUE,
    name        VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255),                   -- 本地认证（可选）
    avatar_url  TEXT,
    status      VARCHAR(20) NOT NULL DEFAULT 'active',  -- active/disabled
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 用户组（对接组织架构）
CREATE TABLE groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id    UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

-- 服务账号（供 API Token 绑定）
CREATE TABLE service_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 统一身份视图（用户 + 服务账号），供 Token/Permission 引用
-- 用 polymorphic 关联：identity_type + identity_id
```

### 2.2 迁移 002：工作区与目录

```sql
-- migrations/002_workspaces.up.sql

-- 工作区
CREATE TABLE workspaces (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    slug        VARCHAR(255) NOT NULL UNIQUE,
    description TEXT,
    owner_id    UUID NOT NULL REFERENCES users(id),
    settings    JSONB NOT NULL DEFAULT '{}',      -- 工作区级配置
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 目录（无限极树）
CREATE TABLE directories (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    parent_id     UUID REFERENCES directories(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    path          LTREE NOT NULL,                  -- 物化路径，如 root.dir1.dir2
    sort_order    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_directories_workspace ON directories(workspace_id);
CREATE INDEX idx_directories_parent ON directories(parent_id);
CREATE INDEX idx_directories_path ON directories USING GIST(path);  -- ltree GIST 索引
CREATE INDEX idx_directories_workspace_path ON directories(workspace_id, path);
```

### 2.3 迁移 003：文档与块

```sql
-- migrations/003_documents.up.sql

-- 文档
CREATE TABLE documents (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    directory_id  UUID REFERENCES directories(id) ON DELETE SET NULL,
    title         VARCHAR(500) NOT NULL,
    content       JSONB NOT NULL DEFAULT '[]',     -- Block 数组 JSON
    content_text  TEXT NOT NULL DEFAULT '',        -- 纯文本（用于 FTS 索引）
    format        VARCHAR(20) NOT NULL DEFAULT 'blocks',  -- blocks/markdown
    status        VARCHAR(20) NOT NULL DEFAULT 'draft',   -- draft/published/archived/deleted
    index_status  VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending/processing/indexed/failed
    created_by    UUID NOT NULL REFERENCES users(id),
    updated_by    UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 全文检索索引（中文分词）
CREATE INDEX idx_documents_fts ON documents
    USING GIN(to_tsvector('chinese_zh', coalesce(title,'') || ' ' || coalesce(content_text,'')))
    WHERE status != 'deleted';

CREATE INDEX idx_documents_workspace ON documents(workspace_id) WHERE status != 'deleted';
CREATE INDEX idx_documents_directory ON documents(directory_id) WHERE status != 'deleted';
CREATE INDEX idx_documents_status ON documents(status, index_status);
CREATE INDEX idx_documents_updated ON documents(updated_at DESC);
CREATE INDEX idx_documents_created_by ON documents(created_by);
CREATE INDEX idx_documents_content_gin ON documents USING GIN(content jsonb_path_ops);  -- Block 查询

-- 文档版本
CREATE TABLE document_versions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version_no    INTEGER NOT NULL,
    content       JSONB NOT NULL,
    content_text  TEXT NOT NULL DEFAULT '',
    diff_summary  TEXT,                            -- 变更摘要
    author_id     UUID NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, version_no)
);

CREATE INDEX idx_versions_document ON document_versions(document_id, version_no DESC);
CREATE INDEX idx_versions_created ON document_versions(created_at DESC);

-- 块（可选独立表，也可仅存 JSONB。MVP 存 JSONB，此表为预留结构化查询）
CREATE TABLE blocks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    block_type    VARCHAR(50) NOT NULL,            -- text/code/chart/canvas/heading
    content       JSONB NOT NULL DEFAULT '{}',
    sort_order    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_blocks_document ON blocks(document_id, sort_order);

-- 附件
CREATE TABLE attachments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    name          VARCHAR(500) NOT NULL,
    mime_type     VARCHAR(255) NOT NULL,
    size_bytes    BIGINT NOT NULL,
    storage_key   TEXT NOT NULL,                   -- MinIO object key
    storage_type  VARCHAR(20) NOT NULL DEFAULT 'minio',
    uploaded_by   UUID NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_attachments_document ON attachments(document_id);
```

### 2.4 迁移 004：标签

```sql
-- migrations/004_tags.up.sql

CREATE TABLE tags (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          VARCHAR(100) NOT NULL,
    color         VARCHAR(20),
    parent_id     UUID REFERENCES tags(id) ON DELETE CASCADE,  -- 层级标签
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, name, parent_id)
);

CREATE INDEX idx_tags_workspace ON tags(workspace_id);

-- 文档-标签关联
CREATE TABLE document_tags (
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    tag_id        UUID NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, tag_id)
);

CREATE INDEX idx_doctags_tag ON document_tags(tag_id);
```

### 2.5 迁移 005：RBAC 权限

```sql
-- migrations/005_rbac.up.sql

-- 角色
CREATE TABLE roles (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(100) NOT NULL,
    scope         VARCHAR(20) NOT NULL,            -- system/workspace/directory/page
    workspace_id  UUID REFERENCES workspaces(id) ON DELETE CASCADE,  -- workspace 以下级角色关联
    permissions   JSONB NOT NULL DEFAULT '[]',     -- ["read","write","admin"]
    is_system     BOOLEAN NOT NULL DEFAULT false,  -- 系统内置角色不可删
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 权限授权（主体 → 角色 → 目标资源）
CREATE TABLE permissions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_type  VARCHAR(20) NOT NULL,            -- user/group
    subject_id    UUID NOT NULL,                   -- users.id 或 groups.id
    role_id       UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    target_type   VARCHAR(20) NOT NULL,            -- workspace/directory/document
    target_id     UUID NOT NULL,                   -- 对应资源 ID
    effect        VARCHAR(10) NOT NULL DEFAULT 'allow',  -- allow/deny
    inherit_scope VARCHAR(20) NOT NULL DEFAULT 'subtree', -- node_only/subtree
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    UUID REFERENCES users(id)
);

CREATE INDEX idx_permissions_subject ON permissions(subject_type, subject_id);
CREATE INDEX idx_permissions_target ON permissions(target_type, target_id);
CREATE INDEX idx_permissions_role ON permissions(role_id);
-- 查询某用户在某工作区的所有权限（含继承）
CREATE INDEX idx_permissions_target_inherit ON permissions(target_type, target_id, inherit_scope);
```

### 2.6 迁移 006：审计日志

```sql
-- migrations/006_audit.up.sql

-- 审计日志（追加写，按月分区）
CREATE TABLE audit_logs (
    id            UUID NOT NULL DEFAULT gen_random_uuid(),
    actor_type    VARCHAR(20) NOT NULL,            -- user/service_account/agent/system
    actor_id      UUID,
    action        VARCHAR(100) NOT NULL,           -- document.create/rbac.update/mcp.tool_call/...
    target_type   VARCHAR(50),
    target_id     UUID,
    detail        JSONB NOT NULL DEFAULT '{}',     -- 操作详情
    ip_address    INET,
    user_agent    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- 当月分区（由定时任务自动创建未来分区）
CREATE TABLE audit_logs_2026_07 PARTITION OF audit_logs
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');

CREATE INDEX idx_audit_actor ON audit_logs(actor_type, actor_id);
CREATE INDEX idx_audit_action ON audit_logs(action);
CREATE INDEX idx_audit_target ON audit_logs(target_type, target_id);
CREATE INDEX idx_audit_created ON audit_logs(created_at DESC);
```

### 2.7 迁移 007：RAG 域

```sql
-- migrations/007_rag.up.sql

-- Embedding 模型配置
CREATE TABLE embedding_models (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider          VARCHAR(50) NOT NULL,         -- tei/ollama
    model_name        VARCHAR(255) NOT NULL,
    dimension         INTEGER NOT NULL,             -- 1024 等
    max_token         INTEGER NOT NULL DEFAULT 8192,
    instruction_query TEXT,                         -- 检索 query 前缀
    instruction_doc   TEXT,                         -- 入库 doc 前缀
    status            VARCHAR(20) NOT NULL DEFAULT 'active',  -- active/inactive
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider, model_name)
);

-- 索引任务
CREATE TABLE indexing_tasks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    event_type    VARCHAR(20) NOT NULL,             -- create/update/delete/permission_change
    status        VARCHAR(20) NOT NULL DEFAULT 'pending',  -- pending/processing/indexed/failed
    attempt       INTEGER NOT NULL DEFAULT 0,
    max_attempt   INTEGER NOT NULL DEFAULT 3,
    payload       JSONB NOT NULL DEFAULT '{}',      -- 事件详情
    error_message TEXT,
    model_id      UUID REFERENCES embedding_models(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_indexing_tasks_status ON indexing_tasks(status, updated_at);
CREATE INDEX idx_indexing_tasks_document ON indexing_tasks(document_id);
CREATE INDEX idx_indexing_tasks_pending ON indexing_tasks(status, created_at) WHERE status IN ('pending','failed');

-- Chunk 元数据（向量本体存 Qdrant，此表记录元数据用于对账/补偿）
CREATE TABLE chunks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version_no    INTEGER NOT NULL,
    chunk_index   INTEGER NOT NULL,
    text          TEXT NOT NULL,
    token_count   INTEGER,
    model_id      UUID NOT NULL REFERENCES embedding_models(id),
    qdrant_point_id UUID NOT NULL,                  -- Qdrant 中对应 point ID
    metadata      JSONB NOT NULL DEFAULT '{}',      -- {workspace_id,directory_id,tags,visible_to}
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, version_no, chunk_index)
);

CREATE INDEX idx_chunks_document ON chunks(document_id, version_no);
CREATE INDEX idx_chunks_qdrant ON chunks(qdrant_point_id);
CREATE INDEX idx_chunks_model ON chunks(model_id);
```

### 2.8 迁移 008：MCP 域

```sql
-- migrations/008_mcp.up.sql

-- API Token
CREATE TABLE api_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(255) NOT NULL,
    token_hash    VARCHAR(255) NOT NULL UNIQUE,     -- 存哈希，不存明文
    prefix        VARCHAR(20) NOT NULL,             -- 明文前缀（展示用，如 wki_xxxx）
    identity_type VARCHAR(20) NOT NULL,             -- user/service_account
    identity_id   UUID NOT NULL,                    -- users.id 或 service_accounts.id
    scope         VARCHAR(20) NOT NULL DEFAULT 'readonly',  -- readonly/readwrite/admin
    expires_at    TIMESTAMPTZ,                      -- NULL = 永不过期
    revoked_at    TIMESTAMPTZ,                      -- NULL = 未吊销
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    UUID REFERENCES users(id)
);

CREATE INDEX idx_tokens_identity ON api_tokens(identity_type, identity_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_tokens_hash ON api_tokens(token_hash) WHERE revoked_at IS NULL;

-- MCP 会话
CREATE TABLE mcp_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id      UUID NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    transport     VARCHAR(20) NOT NULL,             -- http_sse/stdio
    client_info   JSONB,                            -- Agent 名称/版本
    capabilities  JSONB,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at      TIMESTAMPTZ
);

CREATE INDEX idx_mcp_sessions_token ON mcp_sessions(token_id, started_at DESC);

-- MCP 工具调用记录
CREATE TABLE mcp_tool_calls (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id    UUID NOT NULL REFERENCES mcp_sessions(id) ON DELETE CASCADE,
    tool_name     VARCHAR(100) NOT NULL,
    params_summary JSONB NOT NULL DEFAULT '{}',     -- 参数摘要（脱敏）
    result_status VARCHAR(20) NOT NULL,             -- success/forbidden/error
    target_resource TEXT,                            -- 操作目标资源标识
    duration_ms   INTEGER,
    audit_log_id  UUID,                              -- 关联 audit_logs.id
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_mcp_calls_session ON mcp_tool_calls(session_id, created_at DESC);
CREATE INDEX idx_mcp_calls_tool ON mcp_tool_calls(tool_name, created_at DESC);
```

### 2.9 迁移 009：评论

```sql
-- migrations/009_comments.up.sql

CREATE TABLE comments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    block_id      UUID,                              -- 锚定块（NULL = 整体评论）
    parent_id     UUID REFERENCES comments(id) ON DELETE CASCADE,  -- 回复
    author_id     UUID NOT NULL REFERENCES users(id),
    content       TEXT NOT NULL,
    mentions      UUID[],                            -- @提及的用户 ID
    resolved      BOOLEAN NOT NULL DEFAULT false,
    resolved_by   UUID REFERENCES users(id),
    resolved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_comments_document ON comments(document_id, created_at DESC);
CREATE INDEX idx_comments_block ON comments(document_id, block_id) WHERE block_id IS NOT NULL;
CREATE INDEX idx_comments_parent ON comments(parent_id);
CREATE INDEX idx_comments_unresolved ON comments(document_id) WHERE resolved = false;
```

---

## 3. Qdrant 向量库设计

### 3.1 Collection 设计

```
Collection: wiki_chunks_{model_id}_{dim}
  例: wiki_chunks_qwen3_1024

向量配置:
  dense向量: size=1024, distance=Cosine
  sparse向量: 启用（用于 BM25 混合检索，Qdrant 原生支持）
```

**注意**：维度变更（切换模型）须新建 Collection，禁止混维度查询。模型切换触发存量重建任务（异步）。

### 3.2 Point Payload 结构（RBAC 可见性核心）

每个 Qdrant point 的 payload：

```json
{
  "document_id": "uuid",
  "workspace_id": "uuid",
  "directory_id": "uuid",
  "version_no": 1,
  "chunk_index": 0,
  "chunk_text": "...",           // 片段文本（用于结果展示）
  "model_id": "uuid",
  "tags": ["tag1", "tag2"],
  "visible_to": [                // RBAC 可见性 payload（核心）
    "user:uuid1",
    "user:uuid2",
    "group:uuid3"
  ],
  "status": "published",         // 文档状态
  "created_at": "2026-07-29T..."
}
```

### 3.3 RBAC Payload 过滤方案

**设计原则**：`visible_to` 数组记录所有对该 chunk 有"读"权限的主体（user ID + group ID）。检索时，将当前用户的 ID 及其所属 group ID 集合作为过滤条件，与 `visible_to` 取交集。

**Qdrant 检索过滤示例**：

```json
{
  "filter": {
    "must": [
      { "key": "workspace_id", "match": { "value": "ws_uuid" } },
      { "key": "status", "match": { "value": "published" } },
      {
        "key": "visible_to",
        "match": {
          "any": ["user:current_user_id", "group:group1_id", "group:group2_id"]
        }
      }
    ]
  },
  "search": {
    "vector": [0.1, 0.2, ...],
    "limit": 50
  }
}
```

**可见性维护**：
- 文档创建/更新时：RAG Worker 根据当前权限计算 `visible_to`，写入 payload。
- 权限变更时：Mora API 发布 `permission_change` 事件 → RAG Worker 重新计算受影响文档所有 chunk 的 `visible_to`，批量更新 Qdrant payload（`set_payload`）。
- 重算完成前：旧 `visible_to` 保守生效（可能少给权限，不会多给）。

### 3.4 混合检索（Dense + Sparse/BM25）

Qdrant 原生支持 Dense + Sparse 混合查询：

```json
{
  "search": {
    "vector": {
      "dense": [0.1, 0.2, ...],
      "sparse": { "indices": [1, 5, 10], "values": [0.3, 0.5, 0.2] }
    },
    "limit": 50
  }
}
```

- Dense 向量：TEI/Ollama 生成的 Embedding。
- Sparse 向量：BM25 词项向量（由 Qdrant 内置 sparse encoder 或外部生成）。
- 融合：Qdrant 内部 RRF 融合，或客户端侧加权融合。
- Reranking（P1）：取 Top-50 送 TEI Cross-Encoder 重排，返回 Top-N。

---

## 4. 索引策略汇总

### 4.1 PostgreSQL 索引

| 表 | 索引 | 类型 | 用途 |
|---|---|---|---|
| documents | idx_documents_fts | GIN(tsvector) | 全文检索（中文分词） |
| documents | idx_documents_workspace | B-tree | 按工作区查询 |
| documents | idx_documents_directory | B-tree | 按目录查询 |
| documents | idx_documents_status | B-tree | 按状态/索引状态查询 |
| documents | idx_documents_updated | B-tree DESC | 按更新时间排序 |
| documents | idx_documents_content_gin | GIN(jsonb) | Block 内容查询 |
| directories | idx_directories_path | GIST(ltree) | 目录树路径查询 |
| directories | idx_directories_workspace_path | B-tree | 工作区内路径查询 |
| permissions | idx_permissions_subject | B-tree | 查用户权限 |
| permissions | idx_permissions_target | B-tree | 查资源权限 |
| permissions | idx_permissions_target_inherit | B-tree | 继承权限查询 |
| audit_logs | idx_audit_actor/action/target/created | B-tree | 审计查询 |
| indexing_tasks | idx_indexing_tasks_pending | B-tree(partial) | 待处理任务 |
| chunks | idx_chunks_document | B-tree | 按文档查 chunk |
| api_tokens | idx_tokens_hash | B-tree(partial) | Token 查找（仅未吊销） |

### 4.2 Qdrant 索引

- **HNSW 索引**：Dense 向量默认 HNSW，`m=16, ef_construct=100`（Qdrant 默认，可调）。
- **Payload 索引**：对 `workspace_id`、`status`、`visible_to`、`document_id` 建 payload 索引，加速过滤。
- **Sparse 向量索引**：Qdrant 自动管理。

### 4.3 全文检索优化

- `zhparser` 中文分词：`to_tsvector('chinese_zh', text)`。
- GIN 索引：支持快速倒排查找。
- 权限过滤：FTS 查询时 JOIN 权限视图，SQL 层过滤不可见文档（存在性不泄露）。
- 性能：10 万文档级 GIN 索引查询 P95 ≤ 1s 可满足。

---

## 5. 数据模型与 PRD §6 一致性对照

| PRD §6 实体 | 表/存储 | 状态 |
|---|---|---|
| Workspace | workspaces | ✅ |
| Directory | directories (ltree) | ✅ |
| Document | documents (JSONB content) | ✅ |
| DocumentVersion | document_versions | ✅ |
| Block | blocks（预留）+ documents.content JSONB | ✅ |
| Attachment | attachments | ✅ |
| Tag | tags + document_tags | ✅ |
| User / Group | users / groups / group_members | ✅ |
| Role | roles | ✅ |
| Permission | permissions | ✅ |
| AuditLog | audit_logs（分区表） | ✅ |
| ApiToken | api_tokens | ✅ |
| IndexingTask | indexing_tasks | ✅ |
| Chunk | chunks（PG 元数据）+ Qdrant points（向量） | ✅ |
| EmbeddingModel | embedding_models | ✅ |
| McpSession | mcp_sessions | ✅ |
| McpToolCall | mcp_tool_calls | ✅ |

---

## 6. 数据量与分区策略

| 表 | 预估数据量（10 万文档） | 分区/归档策略 |
|---|---|---|
| documents | 10 万行 | 不分区 |
| document_versions | 1000 万行（100 版本/文档） | 按时间分区（可选） |
| chunks | 500 万行（50 chunk/文档） | 不分区；向量在 Qdrant |
| audit_logs | 持续增长 | 按月分区，保留 12 月，归档 |
| indexing_tasks | 流水表 | 定期清理已完成（>30 天） |
| mcp_tool_calls | 持续增长 | 按月分区（可选） |

---

> 本数据模型为 Stage 1 门禁交付物，与 PRD §6 实体关系完全一致。迁移脚本可直接用于 Stage 2 研发。
