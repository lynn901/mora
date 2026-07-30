# 系统架构设计

> 文档版本：v1.0 ｜ 产出人：Wiki 知识库架构师 ｜ 对应任务：YS-5
> 技术选型依据：《技术选型与基座决策书》（01-tech-selection-decision.md）

---

## 1. 架构总览

### 1.1 设计目标
- **私有化优先**：全套组件本地部署，默认不出网。
- **事件驱动**：文档变更经消息队列异步驱动 RAG 流水线，Wiki 读写不被向量化阻塞。
- **RBAC 全链路**：权限引擎统一，Wiki 检索、向量库 payload 过滤、MCP 调用均复用同一权限决策。
- **可插拔**：Embedding Provider、Reranker、存储后端、组织架构源抽象为接口。
- **弹性部署**：Docker Compose 单机（试用/≤50 人）+ K8s 生产（多副本/HPA）。

### 1.2 逻辑架构图

```
┌─────────────────────────────────────────────────────────────────────────┐
│                              客户端层                                    │
│  Web 前端 (React)          外部 Agent (MCP Client)      移动端 (只读)    │
└──────┬──────────────────────────┬──────────────────────────┬───────────┘
       │ HTTPS                    │ HTTP/SSE (MCP)            │ HTTPS
       │ REST + WebSocket(Yjs)    │                           │ REST
┌──────▼──────────┐  ┌───────────▼──────────┐  ┌────────────▼───────────┐
│  Wiki API 服务   │  │  MCP Server (Go)     │  │  (共享 Wiki API)        │
│  (Go/Gin)        │  │  - Resources/Tools   │  │                         │
│                  │  │  - 鉴权/审计/限流     │  │                         │
│  - 文档/目录CRUD │  │  - RBAC 透传         │  │                         │
│  - RBAC 引擎     │◄─┤  - 草稿/审阅态       │  │                         │
│  - 版本历史      │  │                      │  │                         │
│  - 全文检索      │  └──────────┬───────────┘  └────────────────────────┘
│  - 协同(Yjs Hub) │             │ 内部 HTTP
│  - 事件发布      │             │
└──┬──┬──┬────────┘             │
   │  │  │                       │
   │  │  │  ┌────────────────────▼──────────────┐
   │  │  │  │  RAG 检索服务 (可独立或嵌入 Wiki)   │
   │  │  │  │  - Dense + BM25 混合召回            │
   │  │  │  │  - RRF 融合 + Reranking            │
   │  │  │  │  - RBAC payload 过滤               │
   │  │  │  └──┬──────────────┬──────────────────┘
   │  │  │     │              │
   │  │  │     ▼              ▼
   │  │  │  ┌──────────┐  ┌──────────────┐
   │  │  │  │ Qdrant   │  │ Postgres FTS │
   │  │  │  │ 向量库    │  │ (BM25 倒排)  │
   │  │  │  └──────────┘  └──────────────┘
   │  │  │
   │  │  │ 事件发布 (Redis Streams)
   │  │  ▼
   │  │  ┌───────────────────────────────────────────┐
   │  │  │  消息队列 (Valkey Streams)                 │
   │  │  │  stream: doc_events                       │
   │  │  └──────────────────┬────────────────────────┘
   │  │                     │ 消费组
   │  │  ┌──────────────────▼────────────────────────┐
   │  │  │  RAG Worker (Go)                          │
   │  │  │  - 文本抽取/清洗 → 切块 → Embedding → 写库 │
   │  │  │  - 级联删除/幂等/重试                     │
   │  │  └──┬─────────────────────────┬──────────────┘
   │  │     │ HTTP                    │ gRPC
   │  │     ▼                         ▼
   │  │  ┌──────────┐          ┌──────────────┐
   │  │  │ TEI /    │          │ Qdrant       │
   │  │  │ Ollama   │          │ (写入向量)    │
   │  │  └──────────┘          └──────────────┘
   │  │
   │  ▼ 数据访问
   │  ┌──────────────┐  ┌──────────┐  ┌──────────┐
   │  │ PostgreSQL   │  │ MinIO    │  │ Valkey   │
   │  │ 元数据/版本   │  │ 附件存储  │  │ 缓存/会话 │
   │  └──────────────┘  └──────────┘  └──────────┘
   │
   ▼ 协同
  ┌──────────────────┐
  │ yjs-server       │
  │ (WebSocket CRDT) │
  └──────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│                            可观测层                                      │
│  Prometheus (指标)  │  Loki (日志)  │  OpenTelemetry (链路追踪)         │
│  Grafana (仪表盘)                                                     │
└─────────────────────────────────────────────────────────────────────────┘
```

### 1.3 组件清单

| 组件 | 职责 | 技术栈 | 副本策略 |
|---|---|---|---|
| **Wiki API** | 文档/目录/RBAC/版本/检索/协同 Hub/事件发布 | Go + Gin | 多副本（K8s） |
| **yjs-server** | 协同编辑 CRDT 同步 + awareness | Node.js（Yjs 生态） | 多副本（按文档分片，粘性路由） |
| **MCP Server** | MCP 协议端点、Resources/Tools、鉴权审计限流 | Go | 多副本 |
| **RAG Worker** | 事件消费、文本处理、切块、Embedding、向量写入/删除 | Go | 多副本（消费组负载均衡） |
| **PostgreSQL** | 元数据、版本、权限、审计、索引任务 | PG 16+ | 主从（K8s）/单实例（Compose） |
| **Qdrant** | 向量存储与检索（Dense+Sparse+payload 过滤） | Rust | 单实例（MVP）/集群（大规模） |
| **MinIO** | 附件/导出文件存储（S3 兼容） | Go | 单实例（MVP）/分布式（生产） |
| **Valkey** | 消息队列（Streams）+ 缓存 + 会话 + 限流 | C | 主从/哨兵（K8s） |
| **TEI** | Embedding + Reranker 推理 | Rust | 单实例（MVP）/多副本（HPA） |
| **Ollama** | 备选 Embedding 推理 | Go | 单实例 |
| **Prometheus** | 指标采集 | Go | 单实例 |
| **Grafana** | 仪表盘 | Go/TS | 单实例 |

---

## 2. 模块划分

### 2.1 Go 后端单体模块化（Modular Monolith）

MVP 阶段采用**模块化单体**（Modular Monolith），而非微服务——降低私有化部署运维复杂度，同时保持模块边界清晰，未来可拆分为微服务。

```
wiki-backend/
├── cmd/
│   ├── wiki-api/          # Wiki API 服务入口
│   ├── mcp-server/        # MCP Server 入口
│   └── rag-worker/        # RAG Worker 入口
├── internal/
│   ├── domain/            # 领域模型（实体、值对象）
│   │   ├── document.go
│   │   ├── workspace.go
│   │   ├── directory.go
│   │   ├── permission.go
│   │   ├── version.go
│   │   ├── chunk.go
│   │   └── ...
│   ├── module/            # 业务模块（按 PRD 四大模块划分）
│   │   ├── wiki/          # 模块一：协同 Wiki
│   │   │   ├── handler/   # HTTP handler
│   │   │   ├── service/   # 业务逻辑
│   │   │   ├── repository/# 数据访问
│   │   │   └── event/     # 事件发布
│   │   ├── rag/           # 模块二：RAG 与向量引擎
│   │   │   ├── pipeline/  # 流水线（抽取/切块/embed/写库）
│   │   │   ├── provider/  # Embedding Provider 抽象（TEI/Ollama）
│   │   │   ├── search/    # 混合检索（Dense+BM25+RRF+Rerank）
│   │   │   └── worker/    # 事件消费者
│   │   ├── mcp/           # 模块三：MCP Server
│   │   │   ├── server/    # MCP 协议实现
│   │   │   ├── resource/  # Resources 实现
│   │   │   ├── tool/      # Tools 实现
│   │   │   └── auth/      # Token 鉴权/审计/限流
│   │   └── platform/      # 模块四：平台基础设施
│   │       ├── rbac/      # RBAC 引擎（全局共享）
│   │       ├── audit/     # 审计日志
│   │       ├── storage/   # 文件存储抽象（MinIO）
│   │       ├── config/    # 配置管理
│   │       └── observ/    # 可观测性
│   ├── infra/             # 基础设施实现
│   │   ├── postgres/      # PG 实现 repository
│   │   ├── qdrant/        # Qdrant 客户端
│   │   ├── minio/         # MinIO 客户端
│   │   ├── valkey/        # Valkey/Redis 客户端
│   │   └── mq/            # 消息队列实现
│   └── pkg/               # 公共工具
│       ├── auth/
│       ├── pagination/
│       └── errors/
├── migrations/            # 数据库迁移脚本
├── api/                   # OpenAPI 契约
├── deployments/
│   ├── docker-compose.yml
│   └── helm/              # K8s Helm Chart
└── go.mod
```

### 2.2 模块职责与依赖

```
                    ┌──────────┐
                    │ platform │ (RBAC/审计/存储/配置) — 基础，无外部依赖
                    └────┬─────┘
         ┌───────────────┼───────────────┐
         │               │               │
    ┌────▼────┐    ┌─────▼─────┐   ┌────▼────┐
    │  wiki   │    │   rag     │   │   mcp   │
    │ (模块一) │    │ (模块二)  │   │ (模块三) │
    └────┬────┘    └─────┬─────┘   └────┬────┘
         │               │               │
         │    事件发布    │               │ RBAC 透传
         └──────►MQ◄──────┘               │
                │                         │
                ▼                         │
           rag-worker                     │
                                        调用
         mcp ─────────────────────────► wiki (REST)
         mcp ─────────────────────────► rag/search (REST)
```

**依赖规则**：
- `platform` 模块为公共基础，被所有业务模块依赖，不反向依赖业务模块。
- `wiki` 模块发布事件到 MQ，不直接调用 `rag`；`rag` 消费事件，单向依赖。
- `mcp` 模块通过内部 REST 调用 `wiki` 与 `rag/search`，复用其能力，不重复实现业务逻辑。
- RBAC 引擎位于 `platform/rbac`，`wiki`/`mcp`/`rag`（payload 过滤时）均调用同一权限决策接口。

---

## 3. 部署架构

### 3.1 Docker Compose 单机部署（试用/≤50 人）

```yaml
# deployments/docker-compose.yml （结构示意，完整版在实现阶段产出）
services:
  wiki-api:        # Wiki API + 协同 Hub（单容器）
    image: wiki-api:latest
    ports: ["8080:8080"]
    depends_on: [postgres, valkey, minio, qdrant]
    environment:
      - DATABASE_URL=postgres://...
      - VALKEY_URL=valkey://valkey:6379
      - MINIO_ENDPOINT=minio:9000
      - QDRANT_URL=http://qdrant:6333
      - TEI_URL=http://tei:8080

  mcp-server:
    image: mcp-server:latest
    ports: ["8081:8081"]
    depends_on: [wiki-api]
    environment:
      - WIKI_API_URL=http://wiki-api:8080

  rag-worker:
    image: rag-worker:latest
    depends_on: [postgres, valkey, qdrant, tei]
    environment:
      - DATABASE_URL=postgres://...
      - VALKEY_URL=valkey://valkey:6379
      - QDRANT_URL=http://qdrant:6333
      - TEI_URL=http://tei:8080
      - EMBEDDING_PROVIDER=tei
      - EMBEDDING_MODEL=Qwen3-Embedding
      - EMBEDDING_DIM=1024

  yjs-server:
    image: yjs-server:latest
    ports: ["8082:8082"]
    depends_on: [valkey]

  postgres:
    image: postgres:16
    volumes: ["pg_data:/var/lib/postgresql/data"]
    environment: ["POSTGRES_DB=wiki", "POSTGRES_PASSWORD=..."]

  qdrant:
    image: qdrant/qdrant:1.8
    volumes: ["qdrant_data:/qdrant/storage"]

  minio:
    image: minio/minio:latest
    volumes: ["minio_data:/data"]
    command: server /data

  valkey:
    image: valkey/valkey:7.2
    volumes: ["valkey_data:/data"]

  tei:
    image: ghcr.io/huggingface/text-embeddings-inference:1.5
    volumes: ["tei_data:/data"]   # 模型文件本地挂载
    command: ["--model-id", "Qwen/Qwen3-Embedding-0.6B"]

  prometheus:
    image: prom/prometheus
    volumes: ["./prometheus.yml:/etc/prometheus/prometheus.yml"]

  grafana:
    image: grafana/grafana
    ports: ["3000:3000"]

volumes:
  pg_data:
  qdrant_data:
  minio_data:
  valkey_data:
  tei_data:
```

**单机部署特点**：
- 一键 `docker compose up -d` 拉起全部组件，初始化 ≤ 30 分钟。
- 所有数据卷本地挂载（`./data` 或 named volume），100% 私有化。
- TEI 模型文件预下载本地挂载，不出网拉取模型。
- 资源建议：8 核 CPU / 16GB RAM / 100GB 磁盘（含模型）。

### 3.2 Kubernetes 生产部署

```
┌─────────────────── K8s Cluster ───────────────────┐
│                                                    │
│  ┌─── Ingress (TLS 终止) ──────────────────────┐  │
│  │  wiki.example.com → wiki-api Service         │  │
│  │  mcp.example.com  → mcp-server Service       │  │
│  └──────────────────────────────────────────────┘  │
│                                                    │
│  ┌─── 应用层 (Deployment + HPA) ────────────────┐  │
│  │  wiki-api    (replicas: 2-10, HPA on CPU)    │  │
│  │  mcp-server  (replicas: 2-6, HPA on CPU)    │  │
│  │  rag-worker  (replicas: 2-8, HPA on 队列深度)│  │
│  │  yjs-server  (replicas: 2, 粘性路由)         │  │
│  └──────────────────────────────────────────────┘  │
│                                                    │
│  ┌─── 数据层 (StatefulSet) ─────────────────────┐  │
│  │  postgres    (主从, PG Operator)             │  │
│  │  valkey      (主从+哨兵)                     │  │
│  │  qdrant      (单实例 MVP / 集群 P1)          │  │
│  │  minio       (分布式模式)                    │  │
│  └──────────────────────────────────────────────┘  │
│                                                    │
│  ┌─── 推理层 ───────────────────────────────────┐  │
│  │  tei          (replicas: 1-3, GPU 可选)      │  │
│  └──────────────────────────────────────────────┘  │
│                                                    │
│  ┌─── 可观测层 ─────────────────────────────────┐  │
│  │  prometheus / grafana / loki                 │  │
│  └──────────────────────────────────────────────┘  │
│                                                    │
│  ┌─── 存储 ─────────────────────────────────────┐  │
│  │  PVC (持久卷) → 本地磁盘 / 私有 NFS / CSI    │  │
│  └──────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────┘
```

**K8s 部署要点**：
- **Helm Chart**：`deployments/helm/wiki-platform/`，values.yaml 定制各环境配置。
- **健康检查**：每个 Deployment 配置 livenessProbe（`/healthz`）+ readinessProbe（`/ready`）。
- **优雅停机**：`terminationGracePeriodSeconds: 30`，Wiki API 优雅关闭连接，RAG Worker 处理完当前消息后退出。
- **滚动升级**：`strategy: RollingUpdate`，`maxSurge: 1`，`maxUnavailable: 0`。
- **配置注入**：ConfigMap（非敏感）+ Secret（密钥）→ 环境变量。
- **HPA**：Wiki API/MCP 按 CPU；RAG Worker 按自定义指标（队列深度，需 Prometheus Adapter）。
- **资源配额**：各容器设 requests/limits，防资源争抢。
- **网络策略**：NetworkPolicy 限制仅 Ingress 可达应用层，应用层仅可达数据层。

---

## 4. 数据流架构

### 4.1 文档创作到知识可检索（核心闭环）

```
用户编辑文档 → Wiki API 保存到 PostgreSQL
                ↓ 同步发布事件
            Valkey Streams (doc_events)
                ↓ 异步消费
            RAG Worker:
                1. 文本抽取与清洗（从 Block JSON 提取纯文本）
                2. 切块（chunk 512 token, overlap 64, 尊重标题边界）
                3. 调用 TEI/Ollama 获取 Embedding
                4. 写入 Qdrant（payload 含 RBAC 可见性元数据）
                5. 更新文档 index_status → indexed
                ↓
            索引就绪，查询者/Agent 可检索
```

### 4.2 检索数据流

```
查询者/Agent 发起检索
    ↓
Wiki API / MCP Server 接收请求
    ↓
RBAC 引擎计算用户可见的 workspace_id/directory_id/document_id 集合
    ↓
并行执行:
    ├── Dense 检索: Qdrant 向量检索 + payload 过滤（visible_to 交集）
    └── BM25 检索: PostgreSQL FTS + RBAC SQL 过滤
    ↓
RRF 融合（或加权得分）取 Top-50
    ↓
Reranking（TEI Cross-Encoder，可选，P1）→ Top-N
    ↓
返回结构化结果（文档元数据 + chunk 片段 + 得分 + 来源链接）
```

### 4.3 MCP 调用数据流

```
Agent 持 Token → MCP Server
    ↓ initialize
MCP Server 校验 Token（查 ApiToken 表）→ 返回 capabilities
    ↓ tools/call search_knowledge_base
MCP Server 提取 Token 绑定身份 → 调用 RBAC 引擎
    ↓
MCP Server → RAG 检索服务（携带可见范围 payload）
    ↓
返回结果（无权限命中不返回，防存在性泄露）
    ↓ 审计
MCP Server 记录 McpToolCall + AuditLog
```

---

## 5. 依赖组件选型详解

### 5.1 关系数据库：PostgreSQL 16+

**用途**：元数据（Workspace/Directory/Document/Block/Version/Permission/Tag/User/Role/AuditLog/IndexingTask/EmbeddingModel/ApiToken/McpSession/McpToolCall）+ 全文检索（BM25）。

**关键特性利用**：
- **JSONB**：Document.content（Block 数组）以 JSONB 存储，支持 JSON 路径查询。
- **tsvector + zhparser**：全文检索，`to_tsvector('chinese_zh', content_text)` + GIN 索引。
- **LTREE**（可选）：Directory 树路径，`path` 列用 ltree 类型，支持 `@>` 子树查询。
- **MVCC**：高并发读写无锁冲突。
- **分区表**：AuditLog、IndexingTask 按时间分区，便于归档。

**索引策略**：见《数据模型设计》（03-data-model.md）。

### 5.2 向量数据库：Qdrant 1.8+

**用途**：Chunk 向量存储与检索。

**关键特性利用**：
- **Collection**：按 EmbeddingModel 维度建 Collection（禁止混维度）。
- **Payload**：每个 point 携带 `{document_id, workspace_id, directory_id, version_no, chunk_index, visible_to[], tags[], model_id}`。
- **Payload 过滤**：检索时 `must` 条件过滤 `visible_to` 交集（RBAC 硬过滤）。
- **Sparse vectors**：Qdrant 原生支持稀疏向量（BM25 词项），混合检索在 Qdrant 内完成。
- **写入**：RAG Worker 批量 upsert points。
- **删除**：按 `document_id` payload 过滤批量删除（级联清理）。

### 5.3 文件存储：MinIO

**用途**：附件、导入源文件、导出文件。

**关键操作**：
- 上传：Wiki API 生成 `storage_key`（`workspace_id/document_id/attachment_id/filename`），存入 MinIO，元数据记录到 PostgreSQL。
- 下载：预签名 URL（有效期可配），或经 Wiki API 代理下载（鉴权后）。
- 删除：文档删除时级联删除 MinIO 对象（异步补偿）。

### 5.4 消息队列：Valkey Streams

**用途**：文档变更事件驱动 RAG 流水线。

**Stream 设计**：
```
stream: doc_events
fields: { event_id, event_type(create/update/delete/permission_change),
          document_id, workspace_id, version_no, timestamp }
```

**消费组**：
```
group: rag_pipeline_group
consumers: rag-worker-1, rag-worker-2, ...
```

**可靠性**：
- 消费者 ACK 后才从 PEL 移除；崩溃后未 ACK 消息可被其他消费者 claim。
- 幂等：RAG Worker 以 `event_id` 去重（Valkey SET 记录已处理 event_id，TTL 24h）。
- 死信：重试 3 次失败的消息移入 `doc_events:dead` stream，告警。

### 5.5 缓存与会话：Valkey

**用途**：
- 会话：用户会话 Token → 用户 ID 映射（TTL 30 分钟）。
- 热点缓存：文档元数据、目录树、权限计算结果（TTL 5 分钟，权限变更时主动失效）。
- 限流：按 Token/用户 ID 的滑动窗口计数器。
- 协同 awareness：yjs-server 利用 Valkey 跨实例同步 awareness（多副本场景）。

### 5.6 推理引擎：TEI / Ollama

**TEI（主选）**：
- 容器化部署，支持 Qwen3-Embedding 等模型。
- 批量推理 API（`/embed`），适合流水线批量切块向量化。
- Reranker 模式（`/rerank`），支持 Cross-Encoder 重排。
- 模型文件本地挂载，不出网。

**Ollama（备选）**：
- 轻量化，适合小规模/试用场景。
- 支持 embedding 模型（如 `nomic-embed-text`）。
- 通过 Provider 接口切换，不影响上层逻辑。

**Provider 抽象**（见 RAG 设计文档详述）：
```go
type EmbeddingProvider interface {
    Embed(ctx context.Context, texts []string, instruction string) ([][]float32, error)
    Dimension() int
    HealthCheck(ctx context.Context) error
}
```

---

## 6. 组件间通信与配置注入

### 6.1 通信协议矩阵

| 源 → 目标 | 协议 | 端口 | 鉴权 | 说明 |
|---|---|---|---|---|
| 前端 → Wiki API | HTTPS REST | 443/8080 | Session/JWT | 主 API |
| 前端 → yjs-server | WSS | 443/8082 | Session Token | 协同编辑 |
| 前端 → Wiki API | SSE | 443/8080 | Session/JWT | 实时通知 |
| Agent → MCP Server | HTTP/SSE | 443/8081 | Bearer Token | MCP 协议 |
| Wiki API → PostgreSQL | TCP | 5432 | 用户名/密码 | pgx 连接池 |
| Wiki API → Valkey | TCP | 6379 | ACL/密码 | 缓存/MQ |
| Wiki API → MinIO | HTTP/S3 | 9000 | AccessKey/SecretKey | 附件 |
| Wiki API → Qdrant | HTTP/gRPC | 6333/6334 | API Key（可选） | 检索 |
| RAG Worker → Valkey | TCP | 6379 | ACL/密码 | 消费事件 |
| RAG Worker → TEI | HTTP | 8080 | 无（内网） | Embedding |
| RAG Worker → Qdrant | gRPC | 6334 | API Key（可选） | 向量写入 |
| RAG Worker → PostgreSQL | TCP | 5432 | 用户名/密码 | 状态更新 |
| MCP Server → Wiki API | HTTP | 8080 | 内部服务 Token | RBAC/文档 |
| MCP Server → RAG 检索 | HTTP | 8080 | 内部服务 Token | 检索 |
| 所有 → Prometheus | HTTP | 9090 | 无 | 指标拉取 |
| 所有组件 → Loki | HTTP | 3100 | 无 | 日志推送 |

### 6.2 配置注入

**环境变量清单（关键项）**：

```bash
# 数据库
DATABASE_URL=postgres://wiki:***@postgres:5432/wiki?sslmode=disable

# Valkey（消息队列+缓存）
VALKEY_URL=valkey://valkey:6379
VALKEY_PASSWORD=***

# Qdrant
QDRANT_URL=http://qdrant:6333
QDRANT_API_KEY=***

# MinIO
MINIO_ENDPOINT=minio:9000
MINIO_ACCESS_KEY=***
MINIO_SECRET_KEY=***

# TEI
TEI_URL=http://tei:8080
EMBEDDING_PROVIDER=tei
EMBEDDING_MODEL=Qwen/Qwen3-Embedding-0.6B
EMBEDDING_DIM=1024

# MCP
MCP_TRANSPORT=http_sse
MCP_PORT=8081
WIKI_API_URL=http://wiki-api:8080
INTERNAL_SERVICE_TOKEN=***

# 安全
JWT_SECRET=***
TLS_CERT=/etc/tls/tls.crt
TLS_KEY=/etc/tls/tls.key
AUDIT_LOG_PATH=/var/log/wiki/audit.log

# 可观测
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

**注入方式**：
- Docker Compose：`.env` 文件 + `environment` 字段。
- K8s：ConfigMap + Secret → env。

---

## 7. 扩展性与高可用设计

### 7.1 水平扩展

| 组件 | 扩展方式 | 状态管理 |
|---|---|---|
| Wiki API | 无状态，直接加副本 | 会话/缓存走 Valkey |
| MCP Server | 无状态，直接加副本 | Token 验证走 PostgreSQL/Valkey 缓存 |
| RAG Worker | 无状态，消费组负载均衡 | 无本地状态 |
| yjs-server | 按文档分片，粘性路由 | awareness 走 Valkey 共享 |

### 7.2 高可用

| 组件 | MVP（Compose） | 生产（K8s） |
|---|---|---|
| Wiki API | 单实例（降级可用） | 多副本 + HPA |
| PostgreSQL | 单实例 | 主从 + 自动故障转移（PG Operator） |
| Valkey | 单实例 | 主从 + 哨兵 |
| Qdrant | 单实例 | 单实例（MVP）/ 集群（大规模） |
| MinIO | 单实例 | 分布式模式（纠删码） |
| TEI | 单实例 | 多副本 |

### 7.3 可插拔扩展点

| 扩展点 | 接口 | 默认实现 | 备选 |
|---|---|---|---|
| Embedding Provider | `EmbeddingProvider` | TEI | Ollama / 外部 API（需授权） |
| Reranker | `RerankerProvider` | TEI Cross-Encoder | 可选第三方 |
| 文件存储 | `ObjectStorage` | MinIO | S3 兼容存储 |
| 全文检索 | `SearchProvider` | PostgreSQL FTS | Elasticsearch（预留） |
| 组织架构源 | `OrgDirectoryProvider` | 本地用户表 | LDAP/OIDC（预留） |

---

## 8. 架构决策权衡记录

| 决策 | 选择 | 替代方案 | 权衡理由 |
|---|---|---|---|
| 单体 vs 微服务 | 模块化单体 | 微服务 | MVP 运维简单；模块边界清晰可未来拆分 |
| 向量库 | Qdrant | Milvus/pgvector | 私有化运维简单 + 原生 payload 过滤 + 混合检索 |
| 消息队列 | Valkey Streams | Kafka/NATS | 复用缓存组件，运维轻量；事件量级不需 Kafka |
| 全文检索 | Postgres FTS | Elasticsearch | MVP 复用 PG，避免 ES 重型组件；预留抽象可切换 |
| 协同编辑 | Yjs CRDT | 自研 OT | 成熟库降低复杂度；离线/弱网天然支持 |
| 后端语言 | Go | Node.js/Java | 单二进制私有化友好 + 高并发 + 低资源 |

---

> 本架构设计为 Stage 1 门禁交付物。评审通过后，Stage 2 各研发子任务（YS-6/YS-7/YS-8/YS-9）可据此并行启动。
