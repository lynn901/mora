# 技术选型与基座决策书

> 文档版本：v1.0 ｜ 产出人：Mora 知识库架构师 ｜ 对应任务：YS-5
> 父需求：YS-4《团队智能 Mora 与向量知识库平台》｜ PRD v1.0 已通过评审

---

## 1. 决策背景与原则

### 1.1 背景
PRD（YS-4）已通过评审，进入工程化阶段。本决策书为 Stage 1 门禁交付物，须在编码前明确：
- 是自研还是 fork 开源项目作为基座；
- 后端语言/框架、数据库、向量库、消息队列、缓存、推理引擎等核心组件选型；
- 前端技术栈（YS-7 已预定义为 React + TypeScript + shadcn/ui + Tailwind，本决策书确认并约束）。

### 1.2 决策原则
1. **私有化优先**：默认不出网，所有组件须支持本地/私有化部署。
2. **License 合规优先**：若 fork 开源项目，须评估 AGPL/GPL 传染性对私有化交付的影响。
3. **技术栈一致性**：前后端团队维护成本可控，生态成熟，人才易得。
4. **功能覆盖度 vs 改造难度**：基座应覆盖 PRD 核心能力，避免推倒重写；但改造点须可控。
5. **可插拔扩展**：Embedding Provider、Reranker、存储后端、组织架构源须抽象为接口。

### 1.3 关键非功能约束（摘自 PRD §9）
- 全文检索 P95 ≤ 1s（10 万文档）；混合检索含重排 P95 ≤ 800ms（10 万 chunk）。
- 向量化端到端 P95 ≤ 30s；协同编辑操作延迟 ≤ 200ms。
- 单实例 ≥ 200 并发检索；单文档并发协同 ≤ 50 人。
- 默认不出网；审计日志追加写不可篡改；Token 可吊销。
- Docker Compose 单机 ≤ 30 分钟拉起；生产 K8s 多副本无单点。

---

## 2. 基座决策：自研 vs Fork 开源

### 2.1 候选项目评估

从 License、技术栈匹配度、社区活跃度、功能覆盖度、二次改造难度五个维度评估。

| 维度 | Wiki.js | Outline | BookStack | AFFiNE | 自研 |
|---|---|---|---|---|---|
| **License** | AGPLv3（强传染） | MIT（宽松） | MIT（宽松） | 自定义（非 OSI，商用限制多） | 自有，无传染 |
| **技术栈** | Node.js + Vue + Postgres | Node.js + React + Postgres + Redis | PHP + Laravel + MySQL | Rust + TypeScript + Node（多语言） | 自选，全栈统一 |
| **社区活跃度** | 高（GitHub 25k+ star，活跃维护） | 高（30k+ star，活跃） | 中（16k+ star，维护稳定） | 高（超 50k star，但架构重） | N/A |
| **功能覆盖度** | 知识库 + 权限 + 全文检索 + 多语言；无原生 RAG/MCP | 知识库 + 协同（CRDT Yjs）+ 权限；无 RAG/MCP | 知识库 + 权限；架构偏简单，协同弱 | 白板 + 文档 + 块编辑；协同强；无 RAG/MCP | 按需实现 |
| **协同编辑** | 无实时协同（v2.x） | Yjs CRDT，实时协同成熟 | 无实时协同 | CRDT，白板+文档协同强 | 须自研/集成 Yjs |
| **RAG/MCP 改造难度** | 高：无向量化基础，须从零接事件驱动 | 高：无向量化基础，但事件机制清晰 | 极高：架构简单，扩展性弱 | 极高：Rust 内核改造门槛大 | 可从设计原生集成 |
| **私有化合规** | AGPLv3：私有化交付给客户须开源全部衍生代码（网络服务也触发），商用风险高 | MIT：无传染，可闭源私有化交付 | MIT：无传染 | 非 OSI License，商用须付费授权，条款复杂 | 无合规风险 |

### 2.2 评分表（满分 5 分）

| 维度（权重） | Wiki.js | Outline | BookStack | AFFiNE | 自研 |
|---|---|---|---|---|---|
| License 合规（30%） | 1（AGPL 传染） | 5（MIT） | 5（MIT） | 2（非标准） | 5 |
| 技术栈匹配（20%） | 3（Node+Vue，前端栈不匹配 YS-7 React） | 4（Node+React，前端栈匹配） | 2（PHP，偏离） | 3（Rust+TS，复杂） | 5 |
| 社区活跃度（10%） | 4 | 4 | 3 | 4 | N/A |
| 功能覆盖度（20%） | 3 | 3 | 2 | 3 | 5（按 PRD 定制） |
| 改造难度（20%，分高=易） | 2 | 3 | 2 | 1 | 5（原生设计） |
| **加权总分** | **2.5** | **3.8** | **2.8** | **2.6** | **5.0** |

### 2.3 决策结论：自研

**结论：采用自研路线，不 fork 任何开源项目作为基座。**

### 2.4 决策理由

1. **License 合规是硬约束**：本平台面向企业私有化交付。Wiki.js 的 AGPLv3 在网络服务（SaaS 式私有化）场景下触发传染性义务，须开源全部衍生代码，商用风险不可接受。AFFiNE 的非标准 License 同样有商用限制。Outline/BookStack 虽为 MIT，但功能覆盖度与改造难度不占优。
2. **RAG/MCP 须原生集成**：PRD 核心差异化能力（事件驱动向量化、RBAC payload 过滤、MCP Server）在任何候选项目中均无基础，fork 后仍须大规模重写其数据模型与事件链路，"借壳"收益低、改造成本高，且背负原项目架构包袱。
3. **技术栈一致性**：自研可统一后端语言与前端栈（YS-7 已定 React + TS），团队维护成本最低；fork 会引入异构栈（Vue/PHP/Rust），长期维护负担重。
4. **功能可控**：自研可严格按 PRD §6 数据模型落地，RBAC/版本/检索/RAG/MCP 各域契约自洽，Stage 2 并行研发无歧义。
5. **风险可控**：自研工作量虽大，但 PRD 已明确 MVP 范围（M1–M8），且可复用成熟开源组件（见 §3）而非从零造轮子，复杂度可管理。

### 2.5 风险与缓解

| 风险 | 等级 | 缓解措施 |
|---|---|---|
| 自研工作量较大 | 中 | 严格按 MoSCoW 分期，MVP 聚焦 M1–M8；复用成熟开源组件（Postgres、Qdrant、Redis、Yjs）而非自研底层 |
| 协同编辑复杂度高 | 中 | 采用 Yjs（CRDT）成熟库，不自研 OT 算法；MVP 先支持同文档协同，复杂冲突降级为锁定 |
| 全文检索中文分词 | 低 | 复用 Postgres `pg_jieba` 或内置 `zhparser` 扩展，不自研分词 |
| MCP 协议演进 | 低 | 跟踪 MCP 规范版本，抽象协议层，版本兼容声明 |
| 无开源社区背书 | 低 | 以 PRD 验收标准为质量门禁，配套单元/集成测试 |

---

## 3. 核心组件选型

### 3.1 选型总表

| 组件 | 选型 | 版本基线 | 理由 |
|---|---|---|---|
| **后端语言** | Go | 1.22+ | 高并发、编译型、部署简单（单二进制）、生态成熟；性能满足检索/向量化低延迟要求 |
| **后端框架** | Gin | v1.10+ | 轻量 HTTP 框架，中间件生态完善，社区活跃；适合 REST + SSE |
| **ORM/SQL** | sqlc + pgx | sqlc v1.25+, pgx v5 | sqlc 生成类型安全代码，pgx 高性能 Postgres 驱动；避免 ORM 运行时开销 |
| **关系数据库** | PostgreSQL | 16+ | 成熟开源（PostgreSQL License，类 BSD 无传染）；原生全文检索（`tsvector` + `zhparser` 中文分词）；JSONB 支持 Block 存储；MVCC 满足并发 |
| **向量数据库** | Qdrant | 1.8+ | Rust 实现，高性能；原生 payload 过滤（满足 RBAC 硬过滤）；Docker/K8s 友好；Apache 2.0 License；支持 Dense + Sparse（BM25）混合检索 |
| **对象/文件存储** | MinIO | RELEASE.2024+ | S3 兼容，私有化部署；Apache 2.0；支持本地卷挂载；附件/导出文件存储 |
| **消息队列** | Redis Streams | 7.2+（Redis Stack） | 轻量、低延迟；满足事件驱动流水线；无需引入 Kafka 重型组件；同时复用作缓存与会话 |
| **缓存** | Redis | 7.2+ | 会话、热点文档元数据、限流计数器、协同感知 |
| **全文检索（补充）** | PostgreSQL FTS（`zhparser`） | — | MVP 用 Postgres 内置 FTS 满足 P95 ≤ 1s；后期若需更强可平滑引入专用引擎（见 §3.2） |
| **协同编辑** | Yjs（CRDT） + yjs-server | yjs 13+ | 成熟 CRDT 库，前端已有生态；yjs-server 提供 awareness/光标/在线状态 |
| **Embedding 推理** | HuggingFace TEI | 1.5+ | 容器化部署，支持 Qwen3-Embedding 等模型；高性能批量推理；Apache 2.0 |
| **Embedding 推理（备选）** | Ollama | 0.1+ | 轻量化本地推理，支持 embedding 模型；适合小规模/试用场景 |
| **Reranker** | TEI（Cross-Encoder 模式） | 1.5+ | 复用 TEI，支持 BGE-reranker 等模型；P1 阶段引入 |
| **可观测-指标** | Prometheus + Grafana | — | 行业标准；Qdrant/Redis/TEI 均原生暴露 Prometheus 指标 |
| **可观测-日志** | 结构化日志（zerolog）+ Loki | — | Go zerolog 高性能结构化日志；Loki 聚合查询 |
| **可观测-追踪** | OpenTelemetry | — | 分布式追踪，覆盖 Mora→MQ→RAG→向量库 全链路 |
| **前端框架** | React 18 + TypeScript | — | YS-7 已预定义；函数组件 + Hooks |
| **前端 UI** | shadcn/ui + Tailwind CSS | — | YS-7 已预定义；设计 token 可控 |
| **前端编辑器** | TipTap（基于 ProseMirror）+ Yjs 绑定 | — | 块编辑器基础；Yjs CRDT 协同；支持 Markdown 序列化 |
| **MCP SDK** | Go MCP SDK（官方 mark3labs/mcp-go 或自实现） | — | Go 实现 MCP Server；HTTP/SSE + stdio 传输 |

### 3.2 选型关键权衡

#### 3.2.1 向量数据库：Qdrant vs Milvus vs pgvector

| 维度 | Qdrant | Milvus | pgvector |
|---|---|---|---|
| License | Apache 2.0 | Apache 2.0 | PostgreSQL License |
| Payload 过滤 | 原生支持，高性能 | 支持但复杂 | SQL WHERE，灵活但向量检索性能弱 |
| 混合检索（Dense+Sparse） | 原生支持 | 支持 | 需自行组合 |
| 部署复杂度 | 单二进制/单容器，简单 | 依赖 etcd/MinIO/Pulsar，复杂 | 随 Postgres，无额外组件 |
| 性能（10 万 chunk） | 优秀 | 优秀（更大规模更优） | 中等（百万级以上退化） |
| 适合场景 | 中小规模私有化，简单运维 | 大规模生产集群 | 极小规模/原型 |

**决策：Qdrant。** 理由：私有化部署运维简单（单容器），原生 payload 过滤完美匹配 RBAC 硬过滤需求，原生混合检索满足 Dense+BM25，10 万 chunk 规模性能充裕。Milvus 运维过重，pgvector 在混合检索与 payload 过滤上能力不足。

#### 3.2.2 消息队列：Redis Streams vs Kafka vs NATS

| 维度 | Redis Streams | Kafka | NATS JetStream |
|---|---|---|---|
| 运维复杂度 | 低（已复用 Redis） | 高（Broker+ZK/KRaft） | 中 |
| 功能 | 消费组、ACK、持久化 | 完整流处理 | 持久化流 |
| License | Redis Stack（RSALv2/SSPL，需注意） | Apache 2.0 | Apache 2.0 |
| 适合规模 | 中小（万级 TPS） | 大（百万 TPS） | 中大 |

**决策：Redis Streams（Valkey 7.2+ 替代方案）。** 理由：平台已依赖 Redis 做缓存/会话，复用降低运维成本；事件量级（文档 CRUD）远未达 Kafka 适用规模。**注意**：Redis Stack License 变更为 RSALv2/SSPL，私有化交付须评估合规——若需完全规避，采用 **Valkey**（Linux 基金会 fork，BSDV 3-Clause）作为 drop-in 替代。决策书中建议生产环境采用 Valkey。

#### 3.2.3 全文检索：Postgres FTS vs Elasticsearch

**决策：MVP 使用 PostgreSQL 内置 FTS（`zhparser` 中文分词）。** 理由：10 万文档级 Postgres FTS 可满足 P95 ≤ 1s；避免引入 Elasticsearch 的运维负担与资源开销（ES 对内存要求高，私有化不友好）。若后续数据量增长至百万级或检索质量不达标，可平滑引入专用引擎（预留 `SearchProvider` 抽象接口，切换实现即可）。

#### 3.2.4 协同编辑：Yjs vs自研 OT

**决策：Yjs（CRDT）。** 理由：Yjs 是成熟的 CRDT 库，无中心化冲突解决瓶颈，天然支持离线/弱网；前端 TipTap 编辑器有官方 Yjs 绑定；不自研 OT 算法降低复杂度与风险。yjs-server 提供 awareness（光标/在线状态）。

#### 3.2.5 后端语言：Go vs Node.js vs Java

| 维度 | Go | Node.js | Java |
|---|---|---|---|
| 并发性能 | 优秀（goroutine） | 中（事件循环） | 良（线程） |
| 部署 | 单二进制，极简 | 需 Node 运行时 | 需 JVM，内存占用高 |
| 生态（RAG/向量） | 良（Qdrant/Redis/PG 均有 Go 客户端） | 优（AI 生态丰富） | 良 |
| 私有化运维 | 低资源占用 | 中 | 高（JVM 调优） |
| MCP SDK | 良（mcp-go） | 优（官方 SDK） | 良 |

**决策：Go。** 理由：编译型单二进制最适合私有化交付（低运维、低资源）；goroutine 天然适配高并发检索与事件流水线消费者；性能满足低延迟要求。Node.js 虽 AI 生态更丰富，但本平台推理由 TEI/Ollama 外置，后端仅需 HTTP 调用，Go 完全胜任。

### 3.3 组件间通信方式

| 通信路径 | 协议 | 说明 |
|---|---|---|
| 前端 ↔ Mora 后端 | HTTPS / REST / WebSocket | REST API + SSE（实时通知）+ WebSocket（协同编辑 Yjs sync） |
| Mora 后端 → MQ | Redis Streams（Valkey） | 文档变更事件投递 |
| MQ → RAG Worker | Redis Streams 消费组 | 流水线消费 |
| RAG Worker → TEI/Ollama | HTTP（内部） | Embedding/Rerank 调用 |
| RAG Worker → Qdrant | gRPC（内部） | 向量读写 |
| Mora 后端 → Qdrant | gRPC（内部） | 检索查询（复用 RAG 检索服务或直连） |
| MCP Server → Mora/RBAC | HTTP（内部 REST） | 复用 Mora API + RBAC 引擎 |
| MCP Server → RAG 检索 | HTTP（内部） | 调用检索接口 |
| 所有组件 → Prometheus | HTTP /metrics | 指标暴露 |
| 所有组件 → Postgres | TCP（pgx） | 元数据读写 |

### 3.4 配置注入方式

- **Docker Compose**：`.env` 文件 + `environment` 字段注入。
- **K8s**：ConfigMap（非敏感配置）+ Secret（密钥/Token/密码）+ 环境变量注入。
- **配置层级**：默认值（代码内置）→ 环境变量 → 配置文件（可选挂载）。敏感配置仅走环境变量/Secret，不落配置文件。
- **不硬编码**：任何密钥、Token、连接串均通过环境变量注入，代码中仅读取 `os.Getenv`。

---

## 4. 前端技术栈确认

YS-7 已预定义前端栈，本决策书确认并补充约束：

| 项 | 选型 | 说明 |
|---|---|---|
| 框架 | React 18 + TypeScript | 函数组件 + Hooks，严格类型 |
| 构建 | Vite | 快速 HMR，生产构建优化 |
| UI 组件 | shadcn/ui + Tailwind CSS | 设计 token 统一，可定制 |
| 状态管理 | Zustand + React Query | 轻量全局状态 + 服务端状态缓存 |
| 编辑器 | TipTap + Yjs binding | 块编辑、协同编辑、Markdown 序列化 |
| 图表/画板 | Mermaid + Excalidraw | 流程图 + 手绘画板 |
| 路由 | React Router v6 | — |
| HTTP | fetch + 自封装 client | 统一鉴权/错误处理 |

---

## 5. License 合规声明

| 组件 | License | 合规影响 |
|---|---|---|
| Go 标准库 + Gin + sqlc + pgx | BSD/MIT | 无传染，可闭源 |
| PostgreSQL | PostgreSQL License（类 BSD） | 无传染 |
| Qdrant | Apache 2.0 | 无传染 |
| MinIO | Apache 2.0 | 无传染 |
| Valkey（Redis 替代） | BSD-3-Clause | 无传染 |
| TEI（HuggingFace） | Apache 2.0 | 无传染 |
| Yjs | MIT | 无传染 |
| TipTap | MIT | 无传染 |
| shadcn/ui | MIT | 无传染 |
| Tailwind CSS | MIT | 无传染 |
| Mermaid | MIT | 无传染 |
| Excalidraw | MIT | 无传染 |
| Prometheus/Grafana/Loki | Apache 2.0 | 无传染 |

**合规结论**：全部组件均采用宽松 License（MIT/BSD/Apache 2.0/PostgreSQL License），无 AGPL/GPL 传染性风险，**支持闭源私有化商用交付**。采用 Valkey 替代 Redis Stack 规避 RSALv2/SSPL 限制。

---

## 6. 决策结论汇总

| 决策项 | 结论 |
|---|---|
| 基座路线 | **自研**（不 fork 开源项目） |
| 后端 | Go 1.22+ / Gin / sqlc + pgx |
| 关系库 | PostgreSQL 16+（含 zhparser 中文分词） |
| 向量库 | Qdrant 1.8+（原生 payload 过滤 + 混合检索） |
| 文件存储 | MinIO（S3 兼容） |
| 消息队列/缓存 | Valkey 7.2+（Redis Streams 兼容，BSD License） |
| 协同编辑 | Yjs（CRDT）+ yjs-server |
| Embedding | TEI（主）/ Ollama（备），兼容 Qwen3-Embedding |
| Reranker | TEI Cross-Encoder（P1 阶段） |
| 前端 | React 18 + TypeScript + shadcn/ui + Tailwind + TipTap + Yjs |
| MCP | Go MCP Server（HTTP/SSE + stdio） |
| 可观测 | Prometheus + Grafana + Loki + OpenTelemetry |
| License 合规 | 全组件宽松 License，支持闭源私有化商用 |

---

> 本决策书为 Stage 1 门禁交付物，评审通过后 Stage 2（YS-6/YS-7/YS-8/YS-9）可据此并行启动研发。
