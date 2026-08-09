# 安全与可观测设计

> 文档版本：v1.0 ｜ 产出人：Mora 知识库架构师 ｜ 对应任务：YS-5
> 依据：PRD §4 模块四（F4.1/F4.2）、§7 边界与异常、§9 非功能需求（安全/可观测/数据一致性）
> 技术选型：TLS + 审计追加写 + Prometheus/Grafana/Loki/OpenTelemetry，见 01-tech-selection-decision.md

---

## 1. 设计目标

满足 PRD §9 安全与可观测要求，覆盖全链路：
- **安全**：传输 TLS、存储可选加密、RBAC 全链路、审计追加写不可篡改、网络连接可配置、Token 可吊销。
- **可观测**：指标（Prometheus）、日志（结构化 + Loki）、链路追踪（OpenTelemetry）、关键操作审计。
- **数据安全**：备份恢复可演练，数据导出/迁移，100% 私有化。

### 1.1 非功能约束（PRD §9）
| 维度 | 要求 |
|---|---|
| 安全 | 传输 TLS；存储可选加密；RBAC 全链路；审计追加写不可篡改；网络连接可配置；Token 可吊销 |
| 可观测 | Prometheus 指标（索引队列/检索延迟/模型调用/错误率）；结构化日志；关键操作审计 |
| 数据一致性 | Mora 与向量库最终一致；删除级联对账补偿；权限变更触发可见性重算 |
| 可用性 | 生产 K8s 多副本无单点；≥ 99.5%（私有化目标） |

---

## 2. 传输安全（TLS）

### 2.1 TLS 终止策略

| 部署形态 | TLS 终止点 | 说明 |
|---|---|---|
| Docker Compose（试用） | 反向代理（Caddy/Traefik）或 Mora API 内置 | 单证书；可选自签 |
| K8s 生产 | Ingress Controller（cert-manager 自动证书） | Ingress 终止 TLS，内部 HTTP；敏感场景内部也可 mTLS |

### 2.2 加密范围
| 通信路径 | 加密 | 说明 |
|---|---|---|
| 客户端 ↔ Ingress/API | **TLS 1.2+**（强制） | 外部流量必加密 |
| 前端 ↔ yjs-server | **WSS** | 协同编辑 WebSocket 加密 |
| Agent ↔ MCP Server | **TLS** | MCP HTTP/SSE 加密 |
| 内部组件间（API↔PG/Valkey/Qdrant/MinIO/TEI） | 默认 HTTP（同集群内网）；**生产可启用 TLS/mTLS** | K8s NetworkPolicy 隔离；高安全场景内部 mTLS |

### 2.3 证书管理
- **K8s**：cert-manager + Let's Encrypt（按需配置 ACME endpoint）或私有 CA；自动轮转。
- **私有化**：支持自定义证书挂载（`TLS_CERT`/`TLS_KEY` 环境变量指向挂载路径）。
- **HSTS**：Ingress 启用 HSTS 头。

---

## 3. 存储安全

### 3.1 存储加密
| 数据 | 加密方式 | 说明 |
|---|---|---|
| PostgreSQL | 卷级加密（LUKS / PVC encrypted CSI）+ 连接 TLS | 应用层不加密，依赖存储层；PG `sslmode=require` |
| Qdrant | 卷级加密 | 向量数据随磁盘加密 |
| MinIO | 服务端加密（SSE-S3 / SSE-KMS） | 附件加密存储 |
| Valkey | 卷级加密 + ACL 密码 | 连接需密码（`requirepass`/ACL） |
| 附件 | MinIO SSE | 上传即加密 |

- **应用级加密**（P2 C4）：敏感字段（如 Token 哈希已存哈希；可选文档加密）按需启用；MVP 依赖卷加密。
- **密钥管理**：密钥通过 K8s Secret / 环境变量注入，不落配置文件；高安全场景对接外部 KMS（预留）。

### 3.2 数据持久化与挂载（PRD F4.2）
- 所有有状态组件（PG/Qdrant/MinIO/Valkey/TEI 模型）均支持 **PVC / 本地目录挂载**，不强制公有云。
- Compose：named volume 挂载本地 `./data`。
- K8s：StatefulSet + PVC（本地磁盘 / 私有 NFS / CSI）。

---

## 4. RBAC 全链路安全

### 4.1 权限决策统一
- RBAC 引擎位于 `platform/rbac`（见 02-system-architecture.md §2.1），Mora API / MCP Server / RAG 检索均调用同一决策。
- 决策优先级：显式拒绝 > 显式允许 > 继承 > 默认拒绝（PRD F1.4）。

### 4.2 全链路过滤点
| 链路 | 过滤点 | 机制 |
|---|---|---|
| Mora 列表/详情 | SQL 层 | 权限视图 JOIN，不可见文档不返回 |
| 全文检索（BM25） | SQL 层 | `rbac_visible()` 函数过滤 |
| 语义检索（Dense） | Qdrant payload 层 | `visible_to` 交集硬过滤（must 条件） |
| MCP 只读工具 | RBAC 引擎 + 检索层 | 无权返回空集（防存在性泄露） |
| MCP 写工具 | RBAC 引擎 | 无权 403 |
| 协同编辑 | 连接鉴权 | WebSocket 连接校验文档 write 权限 |

### 4.3 权限变更与可见性重算
- 权限变更 → 发布 `permission.change` 事件 → RAG Worker 重算受影响文档 chunk 的 `visible_to`（见 05-rag-pipeline-design.md §4.3.3）。
- 重算过渡期：旧可见性保守生效（宁可少给，不多给）。
- 超级管理员强制接管留审计记录（PRD F1.4 边界）。

### 4.4 越权防护
- 所有数据访问强制经 RBAC，无"绕过权限"后门。
- 越权尝试（forbidden）记审计 + 告警（`mcp_forbidden_total` / `rbac_denied_total`）。
- 只读无权限返回空集，不泄露资源存在性（PRD F1.5、F3.2）。

---

## 5. 审计日志（追加写，不可篡改）

### 5.1 审计范围
全量记录关键操作（PRD §9、F1.4、F3.3）：

| 操作类别 | action 示例 | 记录字段 |
|---|---|---|
| 文档 CRUD | `document.create/update/delete` | actor, target, detail, ip, ua, ts |
| 权限变更 | `rbac.update/revoke` | actor, subject, role, target, effect |
| 敏感操作 | `workspace.delete`, `permission.bulk_update` | 含审批/二次确认标记 |
| MCP 调用 | `mcp.tool_call` | token, tool, params_summary, result_status, target |
| Token 管理 | `token.create/revoke` | actor, identity, scope, expires |
| 登录/认证 | `auth.login/token_use` | actor, ip, ua |
| 模型/索引 | `model.switch/rebuild`, `index.rebuild` | actor, model, scope |

### 5.2 不可篡改实现
- **仅 INSERT**：`audit_logs` 表应用层只写不删不改；PG 角色权限限制（`REVOKE UPDATE, DELETE`）。
- **分区表**：按月分区（见 03-data-model.md §2.6），旧分区可转只读表空间 / 归档冷存储。
- **追加写文件**（可选增强）：关键审计同时写追加日志文件（`AUDIT_LOG_PATH`，append-only，文件权限 0640），双重保障。
- **完整性校验**（可选 P1）：每条审计记录含前一条 hash 的链式 hash（`prev_hash`），防篡改可校验。

### 5.3 审计查询
- 管理后台审计页：按 actor / action / target / 时间区间查询。
- MCP 调用记录：`/mcp/tool-calls` 接口（见 04-api-contract.md §10）。
- 保留策略：默认 12 个月，可配；超期归档冷存储。

---

## 6. 网络连接与出网管控（PRD F4.2）

### 6.1 网络连接策略
- **网络策略**：K8s NetworkPolicy 可按组件职责限制访问范围，并按需配置外部服务访问。
- **Docker Compose**：组件默认通过内部网络通信，API/MCP 经反向代理对外；其他端口和外部连接按部署需求配置。
- **模型推理**：TEI/Ollama 可本地部署，也可配置远程模型服务；模型来源和更新策略由部署方决定。

### 6.2 外部连接配置
| 外部连接场景 | 配置 | 管控 |
|---|---|---|
| TEI/Ollama 拉模型 | 可选 | 模型文件可本地挂载或从配置的模型源获取 |
| 外部 Embedding API | 可选 | 须管理员显式配置 `ExternalAPIProvider` + 授权 + 审计 + 可选内容脱敏 |
| ACME 证书（Let's Encrypt） | 可选 | Ingress 按需连接 ACME endpoint |
| 外部 IdP（LDAP/OIDC） | 可选 P2 C3 | 显式配置，审计同步操作 |
| 版本检查/遥测 | 可选 | 按部署方配置并遵循隐私策略 |

### 6.3 外部连接审计
- 任何外部 API 调用（授权的外部模型/IdP）记录审计：`external_call` action，含目标、数据摘要（脱敏）、结果。
- Prometheus 指标 `external_egress_total` 监控出网调用。

---

## 7. 可观测性

### 7.1 指标（Prometheus）

各组件暴露 `/metrics`（Prometheus 抓取）。

**核心指标清单**：

| 指标 | 类型 | 说明 |
|---|---|---|
| `wiki_http_request_duration_seconds` | histogram | HTTP 请求延迟（按 path/method） |
| `wiki_http_requests_total` | counter | 请求总数（按 status） |
| `rag_indexing_queue_depth` | gauge | 索引队列深度（Valkey Stream len） |
| `rag_indexing_task_duration_seconds` | histogram | 向量化端到端耗时 |
| `rag_indexing_task_status_total` | counter | 任务状态（indexed/failed/dead） |
| `rag_search_duration_seconds` | histogram | 检索延迟（dense/bm25/rerank 分阶段） |
| `rag_search_errors_total` | counter | 检索错误 |
| `embedding_provider_duration_seconds` | histogram | Embedding 调用耗时 |
| `embedding_provider_errors_total` | counter | Embedding 错误 |
| `mcp_tool_calls_total` | counter | MCP 工具调用（按 tool/status） |
| `mcp_tool_call_duration_seconds` | histogram | 工具调用耗时 |
| `mcp_forbidden_total` | counter | 越权拒绝 |
| `mcp_rate_limited_total` | counter | 限流触发 |
| `rbac_denied_total` | counter | RBAC 拒绝 |
| `qdrant_ops_duration_seconds` | histogram | Qdrant 操作耗时 |
| `rag_dead_letter_total` | counter | 死信 |
| `external_egress_total` | counter | 出网调用 |

**告警规则（Grafana / Alertmanager）**：
- 索引队列深度 > 1000 持续 5min
- 向量化 P95 > 30s 持续 10min
- 检索 P95 > 800ms 持续 10min
- Embedding 错误率 > 5% 持续 5min
- 死信增长 > 0
- MCP 越权 > 0（立即告警）
- 组件 down / 健康检查失败

### 7.2 日志（结构化 + Loki）

- **结构化日志**：Go zerolog，JSON 格式，含 `timestamp, level, service, trace_id, span_id, request_id, msg, fields`。
- **采集**：各组件 stdout → Loki（Promtail/Vector 采集）。
- **关联**：`trace_id` 贯穿单次请求全链路（API→MQ→Worker→Qdrant），可在 Loki/Grafana 按 trace 检索。
- **日志级别**：DEBUG/INFO/WARN/ERROR，可配；生产默认 INFO。
- **脱敏**：日志不记录密钥/Token 明文/文档正文（仅记录 ID 与摘要）。

### 7.3 链路追踪（OpenTelemetry）

- **接入**：各组件 OTel SDK，导出到 OTel Collector → Jaeger/Tempo。
- **覆盖链路**：
  - 文档保存 → 事件投递 → RAG Worker → Embedding → Qdrant 写入
  - 检索请求 → RBAC → Dense(Qdrant) + BM25(PG) → RRF → Rerank → 返回
  - MCP 调用 → 鉴权 → RBAC → 检索/文档 → 返回
- **采样**：生产按比例采样（如 10%），错误请求 100% 采样。

### 7.4 健康检查
- 每个服务暴露 `/healthz`（liveness）+ `/ready`（readiness）。
- `/ready` 检查关键依赖连通性（PG/Valkey/Qdrant/TEI）。
- K8s livenessProbe/readinessProbe 配置（见 02-system-architecture.md §3.2）。

### 7.5 Grafana 仪表盘
- 平台总览：请求量/延迟/错误率、组件健康。
- RAG 流水线：队列深度、向量化延迟、任务状态、死信。
- 检索：延迟分阶段、召回数、Rerank 命中。
- MCP：工具调用量、越权、限流、会话数。
- 资源：CPU/内存/磁盘/连接池。

---

## 8. 备份与恢复（PRD F4.2）

### 8.1 备份范围与策略

| 数据 | 备份方式 | 频率 | 保留 |
|---|---|---|---|
| PostgreSQL | `pg_dump` 逻辑备份 + WAL 归档（PITR） | 全量日 1 次 + WAL 持续 | 30 天 |
| Qdrant | 快照（`/snapshots` API） | 日 1 次 | 14 天 |
| MinIO | `mc mirror` 到备用 bucket/卷 | 日 1 次 | 30 天 |
| Valkey | RDB 快照 + AOF | 持续 | 7 天 |
| TEI 模型 | 本地挂载，随卷备份 | 一次性 | — |
| 配置 | ConfigMap/Secret 版本化（Git） | 变更即提交 | 永久 |

### 8.2 备份执行
- **K8s**：CronJob 调度备份脚本，备份产物写专用 PVC / 对象存储（私有 MinIO 备用 bucket）。
- **Compose**：cron + 备份脚本，产物写本地 `./backups`。
- **加密**：备份产物加密（GPG / 存储层加密）。
- **RPO**：生产 ≤ 5min（WAL PITR + Qdrant 快照）。

### 8.3 恢复
- **恢复演练**：定期（季度）从备份恢复到独立环境验证一致性。
- **PG 恢复**：PITR 到指定时间点。
- **向量库一致性**：恢复后 Qdrant 与 PG `chunks` 表对账（见 05-rag-pipeline-design.md §2.5），不一致触发重建。
- **管理后台**：备份状态展示 + 一键恢复入口（PRD F4.2）。

### 8.4 数据导出/迁移（PRD F4.2、AC-23）
- 全量导出工具：导出 PG（pg_dump）+ MinIO（mc mirror）+ Qdrant（snapshot）为可迁移包。
- 可在另一私有实例恢复（导入 PG + MinIO + Qdrant + 重建索引）。
- 导出/迁移不向外部服务发送用户内容。

---

## 9. 数据一致性保障

| 场景 | 一致性机制 |
|---|---|
| Mora ↔ 向量库最终一致 | 事件驱动 + 索引状态徽标；失败重试 + 对账补偿 |
| 删除级联 | 删除事件 → RAG Worker 删 chunk；失败有孤儿向量清理对账任务 |
| 权限变更 → 可见性 | `permission.change` 事件 → `visible_to` 重算；过渡期保守生效 |
| 模型维度变更 | 新建 Collection + 存量重建 + 灰度切换；禁混维度查询 |
| 恢复后一致性 | Qdrant/PG 对账，不一致触发重建 |
| 协同冲突 | Yjs CRDT 自动合并；不可合并锁定冲突块提示人工 |

---

## 10. 其他安全措施

| 项 | 措施 |
|---|---|
| XSS 防护 | 富文本内容服务端净化（允许标签白名单）；前端 DOMPurify；CSP 头 |
| SQL 注入 | sqlc 参数化查询；无字符串拼接 SQL |
| 注入（NoSQL/向量） | Qdrant payload 过滤用结构化条件，不拼接 |
| CSRF | API 走 Bearer Token（非 Cookie），无 CSRF 风险；前端 SameSite Cookie |
| 速率限制 | 用户级 + Token 级（见 04-api-contract.md §16） |
| 密钥管理 | 全部经环境变量/Secret 注入；不硬编码；Token 存哈希不存明文 |
| 依赖安全 | 定期 `govulncheck` 扫描；镜像最小化（distroless/scratch） |
| 最小权限 | 各组件 DB 用户最小权限；PG `REVOKE UPDATE,DELETE` on audit_logs |
| 导入安全 | 扫描恶意脚本并剔除（PRD F1.2）；附件 MIME 校验 |

---

## 11. 与 PRD 一致性对照

| PRD 要求 | 本设计 | 状态 |
|---|---|---|
| F4.2 传输 TLS | §2 | ✅ |
| F4.2 存储可选加密 | §3 | ✅ |
| §9 RBAC 全链路 | §4 | ✅ |
| §9 审计追加写不可篡改 | §5 | ✅ |
| §9 网络连接可配置 | §6 | ✅ |
| §9 Token 可吊销 | §4 + 06-mcp §6 | ✅ |
| §9 Prometheus 指标 | §7.1 | ✅ |
| §9 结构化日志 | §7.2 | ✅ |
| §9 链路追踪 | §7.3 | ✅ |
| F4.2 备份恢复 + RPO ≤ 5min | §8 | ✅ |
| F4.2 数据导出/迁移 | §8.4 | ✅ |
| §9 数据一致性（最终一致/对账/重算） | §9 | ✅ |
| §7 异常: 模型不可用降级 / 越权告警 / 存储写满降级 | §6/§7.1 告警 + 05-rag §5.4 | ✅ |
| AC-22 数据挂载本地/可备份恢复/网络连接可配置 | §3.2/§6/§8 | ✅ |

---

> 本设计为 Stage 1 门禁交付物（交付物 #7）。与 02-system-architecture.md（可观测层/部署）、05-rag-pipeline-design.md（一致性/对账）、06-mcp-server-design.md（审计/限流）共同构成安全与可观测完整方案，Stage 2 各子任务可据此落地。
