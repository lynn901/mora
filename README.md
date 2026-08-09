# Mora

> 协同知识库平台 —— 多人实时协作的文档空间、细粒度权限治理、全文 + 向量混合检索，以及面向 Agent 的 MCP 工具层。

Mora 是一套以「企业内部文档协作 + 个人知识管理」为核心场景的协同平台后端，采用 Go 模块化单体（modular monolith）架构实现。它把知识库产品最关键的几件事一次性做扎实：**块级文档的 Markdown 双向可逆转换**、**多工作区隔离与无限级目录**、**不可改写的版本历史与 Diff 回滚**、**显式优先的 RBAC 权限继承**、**BM25 全文 + 向量 RAG 混合检索**，以及**实时协同编辑（presence / cursor / 并发降级）**。

平台支持自托管、100% 私有化：前端（nginx SPA）、三个 Go 服务（mora-api / rag-worker / mcp-server）、CRDT 协同服务（yjs-server）与基础设施（PostgreSQL / Valkey / Qdrant / TEI）全部以 Docker Compose 一键拉起；网络连接按部署环境配置。

## 目录

- [技术栈](#技术栈)
- [功能概览](#功能概览)
- [架构](#架构)
- [目录结构](#目录结构)
- [快速开始（Docker Compose）](#快速开始docker-compose)
- [本地开发](#本地开发)
- [API 概览](#api-概览)
- [权限模型（RBAC）](#权限模型rbac)
- [RAG 检索流水线](#rag-检索流水线)
- [MCP Server](#mcp-server)
- [配置](#配置)
- [可观测性](#可观测性)
- [生产部署（K8s / Helm）](#生产部署k8s--helm)
- [备份与恢复](#备份与恢复)
- [测试](#测试)
- [故障排查](#故障排查)
- [设计文档](#设计文档)
- [安全](#安全)
- [贡献](#贡献)
- [许可证](#许可证)

## 技术栈

| 层 | 选型 | 说明 |
|----|------|------|
| 语言 / HTTP | Go 1.25 / Gin | 模块化单体，单仓三入口（mora-api / rag-worker / mcp-server） |
| 元数据 / 版本 / 权限 / 审计 | PostgreSQL 16 + pgx v5 | 全文检索用 zhparser（缺失时降级 `simple`） |
| 事件流 | Valkey Streams | 文档变更事件驱动 RAG 索引流水线（mora-api 发布，rag-worker 消费） |
| 向量库 | Qdrant | 文档 chunk 向量存储，集合名前缀 `mora_chunks_` |
| Embedding 推理 | TEI（默认）/ Ollama | 本地化推理，默认 `all-MiniLM-L6-v2`（dim 384） |
| 实时协同 | y-websocket (Yjs) + gorilla/websocket | yjs-server 提供 CRDT 同步，mora-api 提供 presence/cursor 中继与并发准入 |
| 鉴权 | JWT | 用户会话；服务间用 `INTERNAL_SERVICE_TOKEN` |
| 可观测 | Prometheus + Grafana | 可选 profile，自带仪表盘 |

> 目录结构遵循架构设计 `design-docs/02-system-architecture.md` §2.1（modular monolith：domain / platform / module / infra / cmd）。

## 功能概览

- **块级文档**：文档以 Block 为最小单位，支持 Markdown ↔ Block 双向可逆转换（往返不丢信息）。
- **工作空间隔离**：多工作空间 + 无限级目录（ltree path 路径），目录树按权限裁剪。
- **版本管理**：每次编辑产生新版本，版本历史不可改写；支持版本 Diff 与回滚（回滚亦产生新版本，不改写历史）。
- **RBAC 权限**：角色继承（admin / write / read）+ 显式覆盖，决策优先级「显式拒绝 > 显式允许 > 继承 > 默认拒绝」，无权文档不在目录树与检索中（存在性不泄露）。
- **混合检索**：BM25 全文检索（多维筛选 + RBAC 硬过滤）与向量 RAG（RRF 融合）并行，RBAC 为硬约束。
- **实时协同**：多人在线编辑 presence/cursor 广播，单文档并发超限自动降级为「一人编辑、他人只读」。
- **评论与审阅**：文档级评论与评论解决。
- **MCP 工具层**：面向 AI Agent 的 MCP Server，暴露文档检索/读取工具，受 RBAC 与速率限制约束。
- **审计与限流**：审计日志追加写（仅 INSERT，分区表），按用户/域的速率限制。

## 架构

```
                      ┌──────────────┐
   浏览器 (SPA)  ─────▶│  nginx:3010  │
                      └──────┬───────┘
                             │ /api/* 反代
                      ┌──────▼───────┐   WebSocket(presence/cursor)
                      │   mora-api   │◀──────────────────────────────┐
                      │   :8080→8990 │                                │
                      └──┬─────┬──┬──┘                                │
            Valkey Streams │     │ │ SQL                         ┌────▼─────┐
                      ┌────▼─┐   │ │                             │  yjs     │
                      │valkey│   │ │                             │ -server  │ (Yjs CRDT
                      └──────┘   │ │                             │  :1234   │  同步)
                                 │ │                             └──────────┘
                      ┌──────────▼─▼─┐  doc_event   ┌──────────┐
                      │  PostgreSQL  │◀─────────────│rag-worker│ (索引消费者,
                      │   (FTS/RBAC) │              │  :8082   │  chunk→向量)
                      └──────────────┘              └────┬─────┘
                                                         │
                                       ┌─────────────────▼────────────────┐
                                       │ Qdrant (向量)  +  TEI (Embedding)│
                                       └──────────────────────────────────┘

                      ┌──────────────┐  INTERNAL_SERVICE_TOKEN
   AI Agent ──MCP────▶│  mcp-server  │──────────────────────────▶ mora-api
                      │  :8081       │  (受 RBAC + 速率限制约束)
                      └──────────────┘
```

- **mora-api**：主 REST API + WebSocket 协同中继。装配 config → pgx pool → repositories → services → handlers → router。
- **rag-worker**：消费 Valkey Streams 的文档变更事件，执行 chunk/嵌入/写向量库，自托管重试。
- **mcp-server**：MCP 协议服务（HTTP / stdio），通过内部 Token 调用 mora-api，对 Agent 暴露受控工具。
- **yjs-server**：Node.js CRDT 同步服务（y-websocket），承载富文本协同编辑的真身；mora-api 负责其外的 presence/cursor 与并发准入。

## 目录结构

```
cmd/
  mora-api/              服务入口（路由装配、中间件、生命周期）+ wiring.go
  rag-worker/            RAG 索引消费者入口
  mcp-server/            MCP 协议服务入口
internal/
  domain/                领域实体（User / Workspace / Document / Block / Permission / ...）
  pkg/                   errors / response / pagination 通用工具
  platform/              config / auth(JWT) / audit / rbac(引擎) / ratelimit / observ
  module/
    mora/
      content/           Markdown↔Block 双向可逆转换
      service/           纯业务逻辑（文档编排、目录树装配、权限解析、仓库接口）
      version/           版本 Diff
      search/            FTS 查询构建器（BM25 + 多维筛选 + RBAC 硬过滤）
      collab/            协同 Hub（presence/cursor 中继 + 并发上限降级）
      event/             事件发布（Valkey Streams / Noop）
      handler/           HTTP handler + 中间件（auth/audit/ratelimit/CORS）
    rag/
      pipeline/          chunk / extract / config
      provider/          TEI / Ollama / Fake Embedding 提供者
      search/            RRF 融合检索
      worker/            事件消费者
      handler/           RAG 管理 / 重建索引 HTTP handler
    mcp/                 MCP server / tool / resource / auth / audit
  infra/
    postgres/            pgx 仓库实现 + RBAC 适配器 + 检索执行器
    pg/, mq/, qdrant/, ragwiring/   基础设施适配
migrations/              12 套 up/down 迁移（001…012，含 demo 种子）
test/integration/        DATABASE_URL 门控的端到端集成测试
deployments/             Dockerfile / docker-compose / Helm chart / 脚本
design-docs/             8 份设计文档（技术选型 / 架构 / 数据模型 / API / RAG / MCP / 安全 / 命名）
api/                     OpenAPI 规范（rag.yaml 等）
web/                     React 19 + Vite + TipTap + shadcn/ui 前端
```

## 快速开始（Docker Compose）

### 前置条件

- Docker Engine 24+、Docker Compose v2.20+
- 4GB+ 可用内存（含 TEI embedding 模型）
- 磁盘 10GB+（数据持久化）

### 一键启动

```bash
# 1. 配置环境变量（可选，默认值可直接试用）
cp .env.example .env
# 编辑 .env 修改 JWT_SECRET / POSTGRES_PASSWORD / INTERNAL_SERVICE_TOKEN

# 2. 构建并启动全部服务
make up
# 或: docker compose -f deployments/docker-compose.yml -p mora up -d

# 3. 等待健康检查通过（首次需下载 TEI 模型，约 1–5 分钟）
make ps

# 4. 查看启动日志
make logs

# 5. 访问
#   前端界面     http://localhost:3010
#   mora-api    http://localhost:8990  (/healthz  /ready)
#   mcp-server  http://localhost:8081  (/mcp/health)
#   yjs-server  ws://localhost:1234    (CRDT 协同，前端自动连接)
#   默认管理员  admin@mora.local / admin123（迁移 010 种子注入）

# 6. 冒烟验证（端到端检查全部 AC）
make verify
```

### 服务清单

| 服务 | 内部端口 | 宿主端口（默认） | 说明 |
|------|---------|------------------|------|
| frontend | 80 | 3010 | nginx 托管前端 SPA，反向代理 API |
| mora-api | 8080 | 8990 | 主 REST API + 协同 WebSocket |
| rag-worker | 8082 | — | RAG 索引消费者（仅内部健康检查） |
| mcp-server | 8081 | 8081 | MCP 协议服务器 |
| yjs-server | 1234 | 1234 | Yjs CRDT 协同同步 |
| postgres | 5432 | — | 元数据 / 版本 / 权限 / 审计 |
| valkey | 6379 | — | 事件流（RAG 管道驱动） |
| qdrant | 6333 | — | 向量数据库 |
| tei | 8080 | — | 本地 Embedding 推理（默认） |
| migrate | — | — | 一次性 DB 迁移 init 容器 |

> 监控与 Ollama 为可选 profile（见 [可观测性](#可观测性) 与 [配置](#配置)）。

### 验收标准覆盖

| AC | 实现 | 验证 |
|----|------|------|
| AC-1 Markdown/富文本双向可逆 | `content` 包 BlocksToMarkdown / MarkdownToBlocks + RoundTrip | `content` 单测 |
| AC-4 多工作区隔离 + 无限级目录 | `directories`(ltree path) + `service.BuildTree` + WorkspaceRepo | `service` 单测 + 集成 TestAC4 |
| AC-6 版本 Diff + 回滚产生新版本 | `version.Diff` + `DocumentService.Rollback`（只新增版本，不改写历史） | `version` 单测 + 集成 TestAC6 |
| AC-7 RBAC 继承与覆盖 | `rbac.Engine` 决策优先级：显式拒绝>显式允许>继承>默认拒绝 | `rbac` 单测 + 集成 TestAC7 |
| AC-8 全文检索多维筛选 + RBAC 过滤 | `search.Filter.Build`（ts_rank_cd BM25 + 硬过滤 visible 集，空集→空结果不泄露存在性） | `search` 单测 + 集成 TestAC8 |
| 接口符合契约 | handler 按 `design-docs/04-api-contract.md` 路由 | go vet + 编译 |

## 本地开发

后端（mora-api / rag-worker / mcp-server 共享一份 Go 模块）：

```bash
# 单元测试（无需 DB）
go test ./...

# 起一个本地 Postgres 跑集成测试（含 11 套迁移）
DATABASE_URL="host=/tmp port=5433 user=mora dbname=mora sslmode=disable" \
  go test -tags=integration ./test/integration/...

# 直接运行 mora-api（需提供 DATABASE_URL / JWT_SECRET / VALKEY_URL 等）
DATABASE_URL="postgres://mora:pw@localhost:5432/mora?sslmode=disable" \
JWT_SECRET="dev-secret-min-32-chars-change-me-please" \
VALKEY_URL="redis://localhost:6379" \
TEI_URL="http://localhost:8080" \
QDRANT_URL="http://localhost:6333" \
go run ./cmd/mora-api
```

前端（`web/`，React 19 + Vite，dev server 自动代理 `/api` 到 `localhost:8080`）：

```bash
cd web
npm install
npm run dev      # http://localhost:5173，/api → mora-api:8080
npm run typecheck
npm run build
```

> 本地裸跑前端 + 后端时：mora-api 监听 `:8080`（`HTTP_ADDR`），前端 Vite dev server 通过 `vite.config.ts` 的 proxy 把 `/api` 转发过去；协同编辑需额外起 yjs-server。

## API 概览

mora-api 暴露 REST API（前缀 `/api/v1`）与健康端点：

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/auth/login` | 登录获取 JWT |
| GET/POST | `/api/v1/workspaces` | 工作空间列表 / 创建 |
| GET | `/api/v1/workspaces/:id` | 工作空间详情 |
| GET/POST | `/api/v1/workspaces/:id/directories` | 目录树 / 创建目录 |
| DELETE | `/api/v1/directories/:id` | 删除目录 |
| GET/POST | `/api/v1/workspaces/:id/documents` | 文档列表 / 创建 |
| GET/PATCH/DELETE | `/api/v1/documents/:id` | 文档读取 / 更新 / 删除 |
| GET | `/api/v1/documents/:id/versions` | 版本列表 |
| GET | `/api/v1/documents/:id/versions/diff` | 版本 Diff |
| POST | `/api/v1/documents/:id/versions/:version_no/rollback` | 回滚（产生新版本） |
| GET | `/api/v1/documents/:id/index-status` | RAG 索引状态 |
| GET | `/api/v1/search` | 全文检索（BM25 + RBAC 硬过滤） |
| POST | `/api/v1/rag/search` | 向量 RAG 检索 |
| GET/POST | `/api/v1/documents/:id/comments` | 评论列表 / 创建 |
| POST | `/api/v1/comments/:id/resolve` | 解决评论 |
| GET/POST/DELETE | `/api/v1/permissions` | 权限列表 / 授权 / 撤销 |
| POST | `/api/v1/permissions/check` | 权限检查 |
| GET/POST | `/api/v1/admin/embedding-models` | Embedding 模型管理 |
| GET | `/api/v1/users` `/api/v1/roles` | 用户 / 角色列表 |
| GET | `/api/v1/ws/collab/:document_id` | 协同 WebSocket（presence/cursor） |
| GET | `/healthz` `/ready` | 存活 / 就绪（就绪含 PG 连通） |

完整契约见 `design-docs/04-api-contract.md`，RAG 部分另见 `api/rag.yaml`（OpenAPI 3.0）。

## 权限模型（RBAC）

`internal/platform/rbac/engine.go` 实现纯逻辑决策，优先级：
**显式拒绝 > 显式允许 > 继承 > 默认拒绝**。

- `admin` 蕴含 read+write+admin；`write` 蕴含 read。
- 引擎按 target 链（文档 → 目录 → 工作区）从具体到宽泛逐级判定，任一级显式拒绝即拒绝。
- 权限变更通过事件触发 RAG 侧 chunk `visible_to` 重算（mora-api 发布事件，重算在 rag-worker）。
- 无权限文档不在目录树与检索结果中（存在性不泄露）。

## RAG 检索流水线

文档发布/更新 → mora-api 向 Valkey Streams 发布 `doc_event` → rag-worker 消费 →
`pipeline`（提取正文 → 切 chunk）→ `provider`（TEI/Ollama 生成向量）→ 写 Qdrant（`mora_chunks_*` 集合），
chunk 携带 `visible_to` 供检索时 RBAC 硬过滤。检索侧 `rag/search` 用 **RRF（Reciprocal Rank Fusion）**
把 BM25 全文结果与向量召回融合，RBAC 为硬约束：未授权 chunk 不返回、不计入 total。

- Embedding 提供者可切换：`EMBEDDING_PROVIDER=tei|ollama`，默认 `tei`。
- 模型与维度可配：`EMBEDDING_MODEL` / `EMBEDDING_DIM`（默认 `all-MiniLM-L6-v2` / 384）。
- 索引重建：`POST /api/v1/admin/embedding-models/:id/rebuild`。

## MCP Server

`cmd/mcp-server` 提供 MCP（Model Context Protocol）服务，面向 AI Agent 暴露受控工具：

- 协议版本 `2025-06-18`，支持 HTTP 与 stdio 两种传输（`MCP_TRANSPORT=http|stdio`）。
- 工具：文档检索（`search`）、文档列表（`list`）、文档读取（`get_document`）。
- 鉴权：通过 `INTERNAL_SERVICE_TOKEN` 调用 mora-api；读/写分别受限流（`MCP_RATE_LIMIT_READ/WRITE`）。
- 全部工具的可见范围同样受 RBAC 约束，不泄露无权文档。

设计见 `design-docs/06-mcp-server-design.md`。

## 配置

复制 `.env.example` 为 `.env` 按需修改。关键变量：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MORA_API_PORT` | 8990 | mora-api 宿主端口 |
| `MCP_SERVER_PORT` | 8081 | MCP 服务端口 |
| `MORA_FRONTEND_PORT` | 3010 | 前端宿主端口 |
| `YJS_PORT` | 1234 | yjs-server 宿主端口 |
| `POSTGRES_PASSWORD` | change-me-pg-password | PostgreSQL 密码（生产必改） |
| `JWT_SECRET` | change-me-jwt-secret-min-32-chars | JWT 签名密钥（生产必改，≥32 字符） |
| `INTERNAL_SERVICE_TOKEN` | change-me-internal-token | 服务间鉴权 Token（生产必改） |
| `EMBEDDING_PROVIDER` | tei | Embedding 提供者（`tei` / `ollama`） |
| `EMBEDDING_MODEL` | sentence-transformers/all-MiniLM-L6-v2 | Embedding 模型 |
| `EMBEDDING_DIM` | 384 | 向量维度 |
| `FTS_CONFIG` | simple | 全文搜索配置（中文 zhparser 镜像用 `chinese_zh`） |
| `COLLAB_MAX_CONCURRENT` | 50 | 单文档最大并发编辑者 |
| `RATE_LIMIT_DOC_PER_MIN` | 300 | 文档接口速率限制 |
| `RATE_LIMIT_SEARCH_PER_MIN` | 200 | 检索接口速率限制 |

宿主端口冲突时无需改文件，直接用环境变量覆盖：

```bash
MORA_API_PORT=9000 MCP_SERVER_PORT=9001 docker compose -f deployments/docker-compose.yml -p mora up -d
```

## 可观测性

可选监控 profile（Prometheus + Grafana）：

```bash
docker compose -f deployments/docker-compose.yml -p mora --profile monitoring up -d
# Grafana:     http://localhost:3030  (admin / admin，密码见 MONITORING_PASS)
# Prometheus:  http://localhost:9090
```

mora-api 内置 Prometheus 指标（`internal/platform/observ`），仪表盘配置在 `deployments/grafana/`。

可选 Ollama 替代 TEI：

```bash
docker compose -f deployments/docker-compose.yml -p mora --profile ollama up -d
docker compose exec ollama ollama pull nomic-embed-text
# 设置 EMBEDDING_PROVIDER=ollama
```

数据持久化卷：`pg_data` / `valkey_data` / `qdrant_data` / `tei_cache` / `ollama_data` / `prometheus_data` / `grafana_data` / `backup_export`。

## 生产部署（K8s / Helm）

Helm Chart 位于 `deployments/chart/mora/`，含模板：`postgresql` / `valkey` / `qdrant` / `tei` / `migrate-job` / `mora-api` / `rag-worker` / `mcp-server` / `frontend` / `secret-config` / `migrations-cm` / `_helpers`。

```bash
# 1. 构建镜像并推送（单 Dockerfile 多 TARGET）
docker build --build-arg TARGET=mora-api    -t registry/mora-api:latest -f deployments/Dockerfile .
docker build --build-arg TARGET=rag-worker  -t registry/rag-worker:latest -f deployments/Dockerfile .
docker build --build-arg TARGET=mcp-server  -t registry/mcp-server:latest -f deployments/Dockerfile .
docker build -f deployments/Dockerfile.web -t registry/mora-frontend:latest .

# 2. 生成 migration ConfigMap（每次 SQL 迁移变更后需重新生成）
bash deployments/chart/hack/generate-migrations-cm.sh

# 3. 部署
helm install mora ./deployments/chart/mora \
  --set postgresql.auth.password="prod-pg-pass" \
  --set config.jwtSecret="prod-jwt-secret-here" \
  --set config.internalServiceToken="prod-internal-token" \
  --set frontend.ingress.host="mora.example.com" \
  --set frontend.ingress.tls.enabled=true \
  --set image.registry="registry/"
```

Helm 特性：滚动升级（`maxUnavailable: 0`，零宕机）、liveness/readiness 探针、HPA（mora-api 默认 2–10 副本，CPU > 70% 扩容）、每有状态组件独立 PVC、前端 Ingress + TLS（cert-manager）。

### 生产注意事项

1. **必改密钥**：`JWT_SECRET`（≥32 字符）、`POSTGRES_PASSWORD`、`INTERNAL_SERVICE_TOKEN`。
2. **资源分配**：TEI 内存约 2GB+，Qdrant 视数据量调整；宿主端口避免冲突。
3. **模型选择**：默认 `all-MiniLM-L6-v2`（dim 384）轻量；生产推荐更大模型（按需配 `EMBEDDING_DIM`）。
4. **中文搜索**：标准 `postgres:16` 无 zhparser，FTS 使用 `simple` 配置；如需中文分词，用 `pgvector/pgvector:pg16` 或含 zhparser 的自定义镜像，并设 `FTS_CONFIG=chinese_zh`。
5. **TLS**：前端 `nginx.conf` 已内置 TLS 配置模板（注释启用）；K8s Ingress 可配合 cert-manager。

## 备份与恢复

```bash
make backup    # PG dump + Qdrant 快照，输出到 ./backup/YYYY-MM-DD_HHMMSS/
make restore   # 按提示输入备份目录路径
make export    # 全量导出（迁移到另一实例），输出 mora-export-<timestamp>.tar.gz
# 在目标实例导入
./deployments/export.sh import mora-export-20260731.tar.gz
```

可选定时备份 profile：`docker compose --profile backup up -d`（保留天数 `BACKUP_RETENTION_DAYS`）。

## 测试

```bash
# 单元测试（无需 DB）
go test ./...

# 集成测试（需可连通的 Postgres，含 11 套迁移）
DATABASE_URL="host=/tmp port=5433 user=mora dbname=mora sslmode=disable" \
  go test -tags=integration ./test/integration/...

# 一键端到端冒烟（需 compose 已启动）
make verify
```

`make verify` 调用 `deployments/e2e-verify.sh`，覆盖健康检查、登录、文档创建/发布/检索、版本、RBAC、MCP 工具调用等全部 AC。

## 故障排查

```bash
# 查看某服务日志
docker compose -f deployments/docker-compose.yml -p mora logs mora-api
docker compose -f deployments/docker-compose.yml -p mora logs rag-worker
docker compose -f deployments/docker-compose.yml -p mora logs tei

# 检查容器健康状态
docker compose -f deployments/docker-compose.yml -p mora ps

# 单独重启某服务
docker compose -f deployments/docker-compose.yml -p mora restart rag-worker

# 重置全部数据（⚠ 清空，不可恢复）
docker compose -f deployments/docker-compose.yml -p mora down -v
```

常用 Makefile 命令：`build` `up` `down` `logs` `ps` `restart` `config` `verify` `backup` `restore` `export` `reset`。

## 设计文档

`design-docs/` 下的设计文档是本仓实现的设计依据：

1. `01-tech-selection-decision.md` — 技术选型与基座决策书
2. `02-system-architecture.md` — 系统架构设计
3. `03-data-model.md` — 数据模型设计
4. `04-api-contract.md` — API 契约（OpenAPI / RESTful）
5. `05-rag-pipeline-design.md` — RAG 流水线与向量库设计
6. `06-mcp-server-design.md` — MCP Server 设计
7. `07-security-observability.md` — 安全与可观测设计
8. `09-design-system.md` — 设计系统

## 安全

- 全部 SQL 参数化（pgx），FTS 配置名严格白名单（`[a-zA-Z0-9_]`）防注入。
- 审计日志追加写（`audit_logs` 分区表，仅 INSERT）。
- JWT 鉴权 + 按用户/域速率限制；服务间 `INTERNAL_SERVICE_TOKEN`。
- 网络连接按部署环境配置；密钥经环境变量注入，不硬编码。
- RBAC 为检索与目录的硬约束，无权资源存在性不泄露。

## 贡献

欢迎提 Issue 与 PR。提交前请确保 `go test ./...` 与 `go vet ./...` 通过，前端 `npm run typecheck` 通过。

## 许可证

[Apache License 2.0](./LICENSE)。
