# RAG 流水线与向量库设计

> 文档版本：v1.0 ｜ 产出人：Mora 知识库架构师 ｜ 对应任务：YS-5
> 依据：PRD §4 模块二（F2.1/F2.2/F2.3）、§5 交互流程、§6.2 RAG 向量域、§9 非功能需求
> 技术选型：Qdrant 1.8+ + Valkey Streams + TEI/Ollama + PostgreSQL FTS（见 01-tech-selection-decision.md）

---

## 1. 设计目标与非功能约束

### 1.1 目标
实现"文档即知识"：文档创建/更新/删除后，后台通过事件驱动流水线**自动、低延迟、无感**地完成文本抽取、切块、向量化与索引同步，并支持带 RBAC 权限过滤的高性能混合检索。

### 1.2 非功能约束（PRD §9）
| 维度 | 指标 |
|---|---|
| 向量化端到端延迟 | P95 ≤ 30s |
| 混合检索（含重排） | P95 ≤ 800ms（10 万 chunk 级） |
| 全文检索（BM25） | P95 ≤ 1s（10 万文档级） |
| 并发检索 | 单实例 ≥ 200 并发 |
| 可靠性 | 流水线失败自动重试（指数退避，最多 3 次），幂等不产生重复向量 |
| 最终一致 | Mora 与向量库最终一致；删除级联有对账补偿；权限变更触发可见性重算 |
| 私有化 | 默认不出网；Embedding 限定本地 TEI/Ollama，外部模型须显式授权+审计 |
| 可插拔 | Embedding Provider、Reranker、检索后端均可抽象替换 |

### 1.3 设计原则
1. **事件驱动、读写隔离**：Mora 读写不被向量化阻塞；流水线异步消费事件。
2. **RBAC 硬约束**：权限过滤在向量库 payload 层与 SQL 层双重生效，不可被任何参数绕过。
3. **幂等优先**：以 `event_id` + `document_id+version_no+chunk_index` 双重去重。
4. **可补偿**：级联删除/权限重算失败有对账任务兜底，清理孤儿向量。
5. **降级友好**：模型不可用 → 流水线排队不丢事件，检索降级为纯 BM25。

---

## 2. 事件驱动链路

### 2.1 事件源与事件类型

Mora API 在文档/附件/权限变更时，向 Valkey Streams 投递事件。事件是流水线的唯一触发源。

| 事件源 | event_type | 触发时机 | 流水线动作 |
|---|---|---|---|
| 文档 CRUD | `document.create` | 文档首次发布/保存 | 全量切块 + 向量化 + 写入 |
| | `document.update` | 文档内容更新（产生新版本） | 删除旧版本 chunk + 新版本全量切块写入 |
| | `document.delete` | 文档软删除 | 按 document_id 级联删除全部 chunk |
| 附件变更 | `attachment.change` | 附件增删（影响可索引文本） | 触发 document.update（重抽取） |
| 权限变更 | `permission.change` | 工作区/目录/页面级权限变更 | 重算受影响文档 chunk 的 `visible_to` payload（不重新切块） |
| 模型切换 | `model.rebuild` | 管理员切换 Embedding 模型 | 存量向量重建（异步、可暂停/续跑） |

### 2.2 事件结构

投递到 Valkey Stream `doc_events` 的消息字段：

```json
{
  "event_id": "uuid",                  // 幂等键，全局唯一
  "event_type": "document.update",
  "document_id": "uuid",
  "workspace_id": "uuid",
  "directory_id": "uuid",
  "version_no": 5,                     // 文档版本号
  "prev_version_no": 4,                // update 事件携带前一版本（用于删旧 chunk）
  "payload": {},                       // 事件详情（权限变更带 target_type/target_id/scope）
  "timestamp": "2026-07-29T08:00:00Z"
}
```

### 2.3 端到端链路

```
┌──────────┐  保存文档   ┌──────────┐  投递事件   ┌───────────────┐
│  用户/   │───────────▶│ Mora API │──────────▶│ Valkey Streams │
│  Agent   │  (PG 事务) │ (Go/Gin) │  (事务后置) │ stream:       │
└──────────┘            └──────────┘            │ doc_events    │
                                                 └──────┬────────┘
                                                        │ 消费组
                                                        ▼
                        ┌─────────────────────────────────────────┐
                        │  RAG Worker (Go)  — 消费组 rag_pipeline  │
                        │                                          │
                        │  1. 幂等去重 (event_id SET, TTL 24h)     │
                        │  2. 创建/读取 IndexingTask (状态机)       │
                        │  3. 文本抽取与清洗                         │
                        │  4. 切块 (Chunking)                       │
                        │  5. Embedding (TEI/Ollama Provider)       │
                        │  6. 写入 Qdrant (payload 含 RBAC 可见性)   │
                        │  7. 更新 index_status → indexed           │
                        │  8. ACK 事件                              │
                        └─────┬───────────────────┬─────────────────┘
                              │ HTTP              │ gRPC
                              ▼                   ▼
                        ┌──────────┐        ┌──────────────┐
                        │ TEI /    │        │   Qdrant     │
                        │ Ollama   │        │  (向量库)     │
                        └──────────┘        └──────────────┘
                              │
                              ▼  索引就绪回执
                        ┌──────────┐
                        │ Mora API │  更新 documents.index_status
                        │ (PG)     │  → 文档徽标刷新（SSE 推前端）
                        └──────────┘
```

**事务一致性**：Mora API 在 PG 事务内完成文档写入，**事务提交后**再投递事件到 Valkey Streams（使用事务后置钩子 `pgx.AfterCommit`）。若投递失败则记录到 `indexing_tasks(status=pending)` 待补偿扫描器重投，保证事件不丢。

### 2.4 消费组与可靠性

```
stream: doc_events
group:  rag_pipeline_group
consumers: rag-worker-1, rag-worker-2, ... rag-worker-N   (消费组负载均衡)
```

- **ACK 语义**：RAG Worker 完成全部阶段（写 Qdrant + 更新 index_status）后才 `XACK`，否则消息留在 PEL（Pending Entries List）。
- **崩溃恢复**：Worker 崩溃后未 ACK 消息，由其他 Worker 通过 `XAUTOCLAIM` 认领（idle > 60s）。
- **死信**：重试达到 `max_attempt`（3 次）仍失败，移入 `doc_events:dead` stream 并告警（Prometheus `rag_dead_letter_total`），管理员可手动重投或丢弃。
- **幂等**：Worker 以 `event_id` 在 Valkey SET 去重（TTL 24h）；同一事件重复投递不产生重复向量。同时 Qdrant point 用确定 ID（见 §4.3），重复 upsert 为覆盖而非新增。

### 2.5 补偿与对账

| 场景 | 对账机制 |
|---|---|
| 事件投递失败 | 补偿扫描器定时扫 `indexing_tasks(status=pending, updated_at < now-5min)`，重投事件 |
| Qdrant 写入部分失败 | `indexing_tasks(status=failed)` 记录失败 chunk 范围，重试仅补缺失 chunk |
| 孤儿向量（PG chunk 记录已删但 Qdrant 残留） | 每日对账任务：按 document_id 扫 Qdrant，删除 PG 中无对应记录的 point |
| 删除级联失败 | 删除事件失败后，孤儿向量清理任务按 document_id 兜底删除 |
| 权限重算遗漏 | 权限变更事件幂等重算，以最新权限快照为准覆盖 payload |

---

## 3. 流水线阶段详述

### 3.1 状态机

每个文档版本对应一个 `IndexingTask`，状态流转：

```
pending → processing → indexed
                    └→ failed ──(retry ×3)──→ processing
                                  └(max_attempt)→ dead(告警)
```

- `pending`：事件已入队，等待消费。
- `processing`：Worker 已领取，正在处理。
- `indexed`：全部 chunk 写入 Qdrant，index_status 回写文档。
- `failed`：处理出错（Embedding 超时/ Qdrant 不可用），attempt+1，指数退避重试（10s/30s/90s）。
- 超过 `max_attempt` → 移入死信，文档 `index_status=failed`，徽标显示失败，管理后台可见重试入口。

### 3.2 阶段一：文本抽取与清洗

输入：Document.content（JSONB Block 数组）+ 附件文本（PDF/Docx 抽取）。

```
Block JSON → 递归遍历 → 提取纯文本（保留标题层级标记） → 清洗
```

- **提取规则**：按 Block 类型提取可索引文本——`heading`/`paragraph` 取 text 节点；`codeBlock` 取代码（标记为代码语义）；`chart`/`canvas` 取 Mermaid 源码或描述；忽略纯装饰块。
- **清洗**：去除零宽字符、多余空白；保留 Markdown 语义标记（`#`/`-`/``````）供切块识别标题边界。
- **附件文本**：PDF 用 `pdftotext`/`unipdf` 抽取正文；Docx 用 `docx` 解析；图片抽取为附件不参与向量化（除非后续引入多模态）。
- **输出**：`content_text`（纯文本，回写 `documents.content_text` 供 FTS）+ 带结构标记的文本流（供切块）。

### 3.3 阶段二：切块（Chunking）

#### 3.3.1 默认策略
- **固定长度 + 重叠**：`chunk_size = 512 token`，`overlap = 64 token`（PRD F2.1 默认值，可按工作区/模型配置）。
- **尊重标题边界**：遇 `#`/`##`/`###` 标题时优先在此切分，避免跨章节；若单节超过 chunk_size 则在节内按长度续切。
- **语义完整**：不在句子中间切断（按句号/换行对齐切点）。

#### 3.3.2 切块算法
```
1. 将文本按标题切分为 sections（h1/h2/h3 为边界）
2. 对每个 section：
   a. 若 section token ≤ chunk_size → 整段为 1 chunk
   b. 若 section token > chunk_size → 按句切分，累加至 ~chunk_size，保留 overlap
3. 为每个 chunk 附加 section_path 元数据（如 "第三章 > 3.2 接口设计"）
4. 输出 chunks[]，每个含 chunk_index、text、token_count、section_path
```

#### 3.3.3 配置化
切块参数存在 `embedding_models` 或工作区 settings，可配置：
```json
{
  "chunk_size": 512,
  "chunk_overlap": 64,
  "respect_heading_boundary": true,
  "max_chunk_size": 1024
}
```

#### 3.3.4 大文档流式处理
- 单文档 > 50MB 或 chunk > 500 时，分批切块与向量化，**不阻塞**流水线主循环。
- 批量 Embedding：每批 32 chunk 调 TEI `/embed`（batch API），降低 RTT 开销。

### 3.4 阶段三：向量化（Embedding）

调用 EmbeddingProvider（见 §5）将 chunk 文本转为向量。

- **Instruction-Aware**：入库时用 `instruction_doc` 前缀拼接（如 `"passage: "`），检索时用 `instruction_query` 前缀（如 `"query: "`）。Qwen3-Embedding 支持 instruction 模式。
- **超长上下文**：对超长文档（> max_token）在切块前可选长窗口模型整体编码（P2 策略 C2）；MVP 以切块处理。
- **维度一致**：写入 Qdrant 前校验向量维度与 Collection 维度一致，不一致直接拒绝并告警（防混维度）。
- **失败处理**：TEI 超时/不可用 → 任务 `failed`，指数退避重试；模型持续不可用 → 流水线降级排队（事件不丢），检索降级为纯 BM25。

### 3.5 阶段四：写入 Qdrant

- **Point ID 确定性**：`point_id = uuid5(namespace, document_id + version_no + chunk_index)`，保证幂等（重复 upsert 覆盖而非新增）。
- **Payload**：见 §4.2，含 RBAC `visible_to`。
- **批量 upsert**：单文档所有 chunk 一次性 `upsert`（Qdrant 批量写）。
- **旧版本清理**：`document.update` 时先按 `document_id + prev_version_no` payload 过滤删除旧 chunk，再写新版本。
- **删除**：`document.delete` 按 `document_id` payload 过滤批量删除全部 chunk。

### 3.6 阶段五：索引就绪回执

- 写入 Qdrant 成功 → 更新 `documents.index_status = indexed`、`chunks` 表元数据。
- 通过 SSE 推送 `index_status` 变更到前端，文档徽标刷新为"已索引"。

---

## 4. Qdrant 向量库设计

### 4.1 Collection 设计

```
Collection: mora_chunks_{model_slug}_{dim}
  例: mora_chunks_qwen3_0_6b_1024

向量配置:
  dense:  { size: 1024, distance: Cosine }
  sparse: { 启用 }          # BM25 词项向量，Qdrant 原生支持

HNSW:    m=16, ef_construct=100, full_scan_threshold=10000
```

- **一模型一 Collection**：维度变更必须新建 Collection，禁止混维度查询。
- **模型切换**：新模型建新 Collection → 存量重建任务双写 → 灰度切换查询指向 → 下线旧 Collection。
- **Payload 索引**：对 `workspace_id`(keyword)、`status`(keyword)、`document_id`(keyword)、`visible_to`(keyword, 多值)、`tags`(keyword, 多值) 建 payload 索引，加速过滤。

### 4.2 Point Payload 结构（RBAC 可见性核心）

```json
{
  "document_id": "uuid",
  "workspace_id": "uuid",
  "directory_id": "uuid",
  "version_no": 5,
  "chunk_index": 3,
  "chunk_text": "分页采用 page/page_size 参数……",
  "section_path": "第三章 > 3.2 接口设计",
  "model_id": "uuid",
  "tags": ["api", "design"],
  "visible_to": [
    "user:uuid1",
    "user:uuid2",
    "group:uuid3",
    "group:uuid4"
  ],
  "status": "published",
  "created_at": "2026-07-29T08:00:00Z"
}
```

### 4.3 RBAC Payload 过滤方案

**核心思想**：`visible_to` 数组记录所有对该 chunk 有"读"权限的主体标识（`user:<id>` / `group:<id>`）。检索时将当前用户 ID 及其所属 group ID 集合作为过滤条件，与 `visible_to` 取交集——有交集即可见。

#### 4.3.1 可见性计算
文档创建/更新时，RAG Worker 调用 RBAC 引擎计算可见主体集合：
```
visible_to = RBAC.resolveReaders(document_id)
  = 所有满足 "对 document_id 或其祖先(directory/workspace)有 read+ 权限" 的 user + group
  - 显式 deny 优先剔除
```
- 权限继承：directory/workspace 级 `subtree` 权限向下传递到文档。
- 显式拒绝 > 显式允许 > 继承 > 默认拒绝（PRD F1.4）。

#### 4.3.2 检索时过滤

```json
{
  "filter": {
    "must": [
      { "key": "workspace_id", "match": { "value": "ws_uuid" } },
      { "key": "status", "match": { "value": "published" } },
      {
        "key": "visible_to",
        "match": { "any": ["user:current_uid", "group:g1", "group:g2"] }
      }
    ]
  },
  "search": { "vector": [...], "limit": 50 }
}
```

- **硬约束**：`visible_to` 过滤为 `must` 条件，不可被任何请求参数移除/绕过。检索服务在调用 Qdrant 前强制注入当前用户可见集合。
- **存在性不泄露**：无权限命中的 chunk 不返回、不计入 total（与 PRD F1.5 一致）。

#### 4.3.3 可见性维护与重算
| 事件 | 动作 |
|---|---|
| 文档创建/更新 | 写入时按当前权限快照计算 `visible_to` |
| `permission.change` | 重算受影响文档（target 及其子树）所有 chunk 的 `visible_to`，Qdrant `set_payload` 批量更新 |
| 重算过渡期 | 旧 `visible_to` 保守生效（可能少给权限，不会多给）；重算完成后覆盖 |

---

## 5. Embedding Provider 抽象

### 5.1 接口定义

```go
// internal/rag/provider/provider.go

type EmbeddingProvider interface {
    // Embed 批量向量化；instruction 区分 query/doc（Instruction-Aware）
    Embed(ctx context.Context, texts []string, instruction string) ([][]float32, error)
    // 当前模型维度
    Dimension() int
    // 健康检查
    HealthCheck(ctx context.Context) error
    // Provider 标识
    Name() string
}

type RerankerProvider interface {
    Rerank(ctx context.Context, query string, docs []string) ([]ScoredDoc, error)
    HealthCheck(ctx context.Context) error
}
```

### 5.2 实现

| Provider | 实现 | 说明 |
|---|---|---|
| TEI | `TEIProvider` | 调 TEI `/embed`（batch）+ `/rerank`（Cross-Encoder）；主选，高性能批量推理 |
| Ollama | `OllamaProvider` | 调 Ollama `/api/embeddings`；备选，轻量化试用场景 |
| 外部 API | `ExternalAPIProvider` | 预留；须显式授权+审计+脱敏（默认不出网） |

### 5.3 模型配置与热切换

- 配置存 `embedding_models` 表（见 03-data-model.md §2.7）。
- **热切换**：管理员新增模型配置 → 连通性测试 → 设为 active → 触发存量重建任务。
- **存量重建**：异步扫描全部 published 文档，按新模型重新切块+向量化写入新 Collection；支持暂停/续跑（按 document_id 游标）；完成灰度后切换查询指向。
- **过渡期**：双 Collection 并存，查询优先新 Collection，未重建文档回退旧 Collection；禁止混维度单次查询。

### 5.4 推理降级策略
| 故障 | 降级 |
|---|---|
| TEI 不可用 | 流水线排队等待（事件不丢），检索降级为纯 BM25（Postgres FTS） |
| TEI 延迟高 | 批量大小动态调小，超时阈值 30s，超时转 failed 重试 |
| 模型维度不匹配 | 拒绝写入，告警，防止污染 Collection |

---

## 6. 高性能混合检索（Dense + BM25 + RRF + Reranking）

### 6.1 检索流程

```
查询者/Agent 发起 /rag/search(query, filters)
    │
    ▼
RBAC 引擎: 计算当前用户可见 workspace_id/directory_id/document_id 集合 + visible_to 主体集
    │
    ├──────────────────────┬──────────────────────────┐
    ▼                      ▼                          ▼
Dense 检索 (Qdrant)    BM25 检索 (PG FTS)        [可选] 元数据预过滤
vector + payload        to_tsvector + RBAC SQL     workspace/tag/time
must(visible_to 交集)   WHERE 权限视图
Top-K (默认 50)         Top-K (默认 50)
    │                      │
    └──────────┬───────────┘
               ▼
        RRF 融合 (Reciprocal Rank Fusion)
        score = Σ 1/(k + rank_i),  k=60
        取 Top-50
               │
               ▼
        [可选 P1] Reranking (TEI Cross-Encoder)
        query × chunk_text 重排 → Top-N (默认 10)
               │
               ▼
        组装结果（文档元数据 + chunk 片段 + 得分 + 来源链接）
        二次 RBAC 校验（防御性，确保每条结果可见）
```

### 6.2 Dense 检索（Qdrant）

- Query 向量化：用 `instruction_query` 前缀调 EmbeddingProvider。
- 过滤：`must` 条件含 `workspace_id` + `status=published` + `visible_to` 交集（RBAC 硬过滤）。
- Sparse 向量：Qdrant 原生 sparse（BM25 词项），可在单次查询内完成 Dense+Sparse 混合，或客户端分别召回。
- 返回 Top-K（默认 50），含 `chunk_text`、`score`、payload。

### 6.3 BM25 检索（PostgreSQL FTS）

```sql
SELECT d.id, d.title, ts_headline('chinese_zh', d.content_text, q) AS snippet,
       ts_rank_cd(fts, q) AS bm25_score
FROM documents d, plainto_tsquery('chinese_zh', $1) q
WHERE d.fts @@ q
  AND d.status = 'published'
  AND d.workspace_id = ANY($2)             -- RBAC: 可见工作区
  AND rbac_visible(d.id, $3, $4)            -- RBAC: 权限视图（用户+组）
  AND ($5::uuid IS NULL OR d.directory_id = $5)
ORDER BY bm25_score DESC
LIMIT $6;
```
- `zhparser` 中文分词；GIN 索引加速。
- RBAC：SQL 层过滤不可见文档（与向量层双重保障）。
- 性能：10 万文档 GIN 查询 P95 ≤ 1s。

### 6.4 RRF 融合

```
对 Dense 与 BM25 各自召回结果按 rank 融合：
  score(doc) = Σ_path 1 / (k + rank_path(doc))    k=60
取融合分 Top-50。
```
- RRF 无需归一化分数，鲁棒于异质打分体系。
- 可选加权 RRF：`score = w_dense·rrf(dense) + w_bm25·rrf(bm25)`，默认等权。

### 6.5 Reranking（P1）

- 取 RRF Top-50 送 TEI Cross-Encoder（如 `BAAI/bge-reranker-large`）重排。
- 输入：`(query, chunk_text)` 对；输出：相关性分数。
- 按分数排序取 Top-N（默认 10）。
- **降级**：Reranker 失败 → 回退 RRF 融合分排序，不阻断检索。
- **性能**：10 万 chunk 级，Dense+BM25+RRF+Rerank 端到端 P95 ≤ 800ms（PRD §9）。

### 6.6 检索结果结构

```json
{
  "items": [
    {
      "document_id": "uuid",
      "title": "API 设计规范",
      "chunk_text": "分页采用 page/page_size 参数……",
      "chunk_index": 3,
      "section_path": "第三章 > 3.2 接口设计",
      "score": 0.92,
      "dense_score": 0.88,
      "bm25_score": 0.65,
      "rerank_score": 0.94,
      "workspace_id": "uuid",
      "source_url": "/workspaces/{ws}/documents/{doc}"
    }
  ],
  "total": 10
}
```

### 6.7 检索抽象与可插拔

```go
type HybridSearcher interface {
    Search(ctx context.Context, req SearchRequest) (*SearchResult, error)
}
// 默认实现: QdrantDenseSearcher + PGFTSSearcher + RRF + TEReranker
// 可替换: ElasticsearchSearcher（预留，数据量增长后切换）
```
- `SearchProvider` 抽象（见 02-system-architecture.md §7.3），MVP 用 Qdrant+PG FTS，未来可平滑切 ES。

---

## 7. 性能与容量

### 7.1 性能预算（10 万文档 / 500 万 chunk）
| 阶段 | 预算 | 说明 |
|---|---|---|
| Query 向量化 | ~50ms | TEI 单条 query embedding |
| Dense 检索 | ~100ms | Qdrant HNSW + payload 过滤 |
| BM25 检索 | ~150ms | PG GIN + RBAC SQL |
| RRF 融合 | ~5ms | 内存计算 |
| Reranking | ~300ms | TEI Cross-Encoder Top-50 |
| 组装+RBAC 复核 | ~20ms | |
| **合计 P95** | **≤ 800ms** | 满足 PRD §9 |

向量化端到端：抽取+切块 ~2s + Embedding(batch 32) ~5s + Qdrant 写入 ~1s ≈ **P95 ≤ 30s**。

### 7.2 容量与扩展
- Qdrant 单实例承载 500 万 chunk（1024 维）≈ 20GB 内存（HNSW）；超规模可分片/集群。
- RAG Worker 水平扩展：消费组自动负载均衡，HPA 按队列深度扩容。
- Embedding 吞吐：TEI 批量推理，单 GPU/CPU 实例满足增量向量化；存量重建可临时扩 TEI 副本。

---

## 8. 与 PRD 一致性对照

| PRD 要求 | 本设计 | 状态 |
|---|---|---|
| F2.1 事件驱动增量向量化 | §2 事件驱动链路 + §3 流水线 | ✅ |
| F2.1 双向级联删除/更新 | §3.5 旧版本清理 + §2.5 对账 | ✅ |
| F2.1 状态机 pending→processing→indexed/failed + 重试 | §3.1 状态机 | ✅ |
| F2.1 切块 chunk 512/overlap 64 + 标题边界 | §3.3 切块 | ✅ |
| F2.2 Provider 抽象 TEI/Ollama/Qwen3 + Instruction-Aware | §5 Provider 抽象 | ✅ |
| F2.2 模型热切换 + 存量重建 | §5.3 | ✅ |
| F2.3 Dense+BM25 混合 + RRF + Reranking | §6 混合检索 | ✅ |
| F2.3 RBAC payload 过滤（硬约束） | §4.3 | ✅ |
| §9 向量化 P95 ≤ 30s / 检索 P95 ≤ 800ms | §7 性能预算 | ✅ |
| §7 异常: 向量化失败不阻塞 Mora / 模型不可用降级 BM25 | §3.1 + §5.4 | ✅ |
| §7 权限变更触发可见性重算 | §4.3.3 | ✅ |
| §7 删除级联失败补偿 | §2.5 对账 | ✅ |

---

> 本设计为 Stage 1 门禁交付物（交付物 #5）。与 03-data-model.md（Qdrant schema/payload）、04-api-contract.md（/rag/search 接口）共同构成 RAG 模块完整契约，Stage 2 YS-8 可据此并行研发。
