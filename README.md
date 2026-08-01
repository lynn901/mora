# Mora 后端核心（YS-6）

协同 Mora 平台后端，Go 模块化单体实现。依据 Stage 1 交付的 7 份设计文档
（技术选型 / 架构 / 数据模型 / API 契约 / RAG / MCP / 安全可观测）落地。

> 父需求：YS-4 ｜ 依赖：YS-5《技术选型与系统架构设计》（已 done）

## 技术栈（遵循 YS-5 决策）

- **Go 1.22+ / Gin** — HTTP 框架
- **PostgreSQL 16 + pgx** — 元数据 / 版本 / 权限 / 审计 / 全文检索（zhparser 中文分词，缺失降级 simple）
- **Valkey Streams** — 文档变更事件驱动 RAG 流水线（本仓提供 publisher）
- **JWT** — 用户会话鉴权
- **gorilla/websocket** — 协同编辑 presence/cursor 中继 + 并发上限降级
- 目录结构遵循架构设计 §2.1（modular monolith：domain / platform / module / infra / cmd）

## 目录结构

```
cmd/wiki-api/            服务入口（路由装配、中间件、生命周期）
internal/
  domain/                领域实体（User/Workspace/Document/Block/Permission/...）
  pkg/                   errors / response / pagination 通用工具
  platform/              config / auth(JWT) / audit / rbac(引擎) / ratelimit
  module/wiki/
    content/             Markdown↔Block 双向可逆转换（AC-1）
    service/             纯业务逻辑（文档编排、目录树装配、仓库接口）
    version/             版本 Diff（AC-6）
    search/              FTS 查询构建器（BM25 + 多维筛选 + RBAC 硬过滤 AC-8）
    collab/              协同 Hub（presence/cursor + 并发上限降级）
    event/               事件发布（Valkey Streams / Noop）
    handler/             HTTP handler + 中间件（auth/audit/ratelimit/CORS）
    repository/          （接口在 service，实现在 infra/postgres）
  infra/postgres/        pgx 仓库实现 + RBAC 适配器 + 检索执行器
migrations/              9 套 up/down 迁移（与 03-data-model.md 一致）
test/integration/        DATABASE_URL 门控的端到端集成测试
deployments/             Dockerfile + docker-compose
```

## 验收标准覆盖

| AC | 实现 | 验证 |
|----|------|------|
| AC-1 Markdown/富文本双向可逆 | `content` 包 BlocksToMarkdown / MarkdownToBlocks + RoundTrip | `content` 单测 |
| AC-4 多工作区隔离 + 无限极目录 | `directories`(ltree path) + `service.BuildTree` + WorkspaceRepo | `service` 单测 + 集成 TestAC4 |
| AC-6 版本 Diff + 回滚产生新版本 | `version.Diff` + `DocumentService.Rollback`（只新增版本，不改写历史） | `version` 单测 + 集成 TestAC6 |
| AC-7 RBAC 继承与覆盖 | `rbac.Engine` 决策优先级：显式拒绝>显式允许>继承>默认拒绝 | `rbac` 单测 + 集成 TestAC7 |
| AC-8 全文检索多维筛选 + RBAC 过滤 | `search.Filter.Build`（ts_rank_cd BM25 + 硬过滤 visible 集，空集→空结果不泄露存在性） | `search` 单测 + 集成 TestAC8 |
| 接口符合契约 | handler 按 04-api-contract.md 路由 | go vet + 编译 |

## 一键部署（Docker Compose）

### 前置条件

- Docker Engine 24+、Docker Compose v2.20+
- 4GB+ 可用内存（含 TEI embedding 模型）
- 磁盘 10GB+（数据持久化）

### 快速开始

```bash
# 1. 配置环境变量（可选，默认值可直接试用）
cp .env.example .env
# 编辑 .env 修改 JWT_SECRET / POSTGRES_PASSWORD / INTERNAL_SERVICE_TOKEN

# 2. 构建并启动全部服务（8 容器 + 1 init 容器）
make up
# 或: docker compose -f deployments/docker-compose.yml up -d

# 3. 等待健康检查通过（首次需下载 TEI 模型，约 1-5 分钟）
make ps

# 4. 查看启动日志
make logs

# 5. 访问
#   前端界面     http://localhost:3000
#   wiki-api    http://localhost:8990 (/healthz /ready)
#   mcp-server  http://localhost:8081  (/mcp/health)
#   默认管理员  admin@mora.local / admin123

# 6. 冒烟验证
make verify
```

### 服务清单

| 服务 | 内部端口 | 宿主端口(默认) | 说明 |
|------|---------|---------------|------|
| frontend | 80 | 3000 | nginx 托管前端 SPA，反向代理 API |
| postgres | 5432 | - | 元数据 / 版本 / 权限 / 审计 |
| valkey | 6379 | - | 事件流（RAG 管道驱动） |
| qdrant | 6333 | - | 向量数据库 |
| tei | 8080 | - | 本地 Embedding 推理（默认） |
| wiki-api | 8080 | 8990 | 主 REST API |
| rag-worker | 8082 | - | RAG 索引消费者 |
| mcp-server | 8081 | 8081 | MCP 协议服务器 |
| migrate | - | - | 一次性 DB 迁移 init 容器 |

### 环境变量

复制 `.env.example` 为 `.env`，按需修改变量。关键变量：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `WIKI_API_PORT` | 8990 | wiki-api 宿主端口 |
| `MCP_SERVER_PORT` | 8081 | MCP 服务端口 |
| `WIKI_FRONTEND_PORT` | 3000 | 前端宿主端口 |
| `POSTGRES_PASSWORD` | wiki | PostgreSQL 密码（生产必改） |
| `JWT_SECRET` | change-me-in-production | JWT 签名密钥（生产必改） |
| `INTERNAL_SERVICE_TOKEN` | wiki-internal-token | 服务间鉴权 Token（生产必改） |
| `EMBEDDING_MODEL` | sentence-transformers/all-MiniLM-L6-v2 | Embedding 模型 |
| `FTS_CONFIG` | simple | 全文搜索配置（中文需 zhparser） |

### 可选 Profile

**Ollama（替代 TEI）：**

```bash
docker compose -f deployments/docker-compose.yml --profile ollama up -d
# 然后拉取 embedding 模型
docker compose exec ollama ollama pull nomic-embed-text
# 设置 EMBEDDING_PROVIDER=ollama
```

**可观测监控（Prometheus + Grafana）：**

```bash
cp .env.example .env  # 确保已配置
docker compose --profile monitoring up -d
# Grafana: http://localhost:3030 (admin/admin)
# Prometheus: http://localhost:9090
```

### 常用命令（Makefile）

```bash
make build    # 构建镜像
make up       # 启动服务
make down     # 停止服务
make logs     # 查看日志
make ps       # 容器状态
make restart  # 重启
make config   # 验证 compose 配置
make verify   # 冒烟测试（E2E 验证）
make backup   # 数据备份
make restore  # 数据恢复
make export   # 全量导出（迁移）
make reset    # ⚠ 清除数据（确认提示）
```

### 数据持久化

所有数据存储在命名卷中：

- `pg_data` — PostgreSQL 数据文件
- `valkey_data` — Valkey 持久化数据
- `qdrant_data` — Qdrant 向量存储
- `tei_cache` — TEI 模型缓存
- `ollama_data` — Ollama 模型数据（仅 ollama profile）
- `prometheus_data` — 指标数据（仅 monitoring profile）
- `grafana_data` — 仪表盘配置（仅 monitoring profile）

```bash
# 查看卷位置
docker volume inspect deployments_pg_data
```

### 备份与恢复

```bash
# 备份全部数据（PG dump + Qdrant 快照）
make backup
# 输出到 ./backup/YYYY-MM-DD_HHMMSS/

# 从备份恢复
make restore
# 按提示输入备份目录路径

# 全量导出（用于迁移到另一实例）
make export
# 输出 wiki-export-<timestamp>.tar.gz

# 在目标实例导入
./deployments/export.sh import wiki-export-20260731.tar.gz
```

### K8s 生产部署（Helm Chart）

Helm Chart 位于 `deployments/chart/wiki/`，含 10 个模板文件：

| 模板 | 组件 | 功能 |
|------|------|------|
| `postgresql.yaml` | PostgreSQL | Deployment + Service + PVC, liveness/readiness |
| `valkey.yaml` | Valkey | Deployment + Service + PVC |
| `qdrant.yaml` | Qdrant | Deployment + Service + PVC |
| `tei.yaml` | TEI | Deployment + Service + PVC, 模型参数注入 |
| `migrate-job.yaml` | DB 迁移 | Helm hook Job (post-install/post-upgrade) |
| `wiki-api.yaml` | Mora API | Deployment + Service + HPA, rolling update |
| `rag-worker.yaml` | RAG Worker | Deployment, rolling update |
| `mcp-server.yaml` | MCP Server | Deployment + Service, rolling update |
| `frontend.yaml` | Nginx 前端 | Deployment + Service + Ingress (TLS 可选) |
| `secret-config.yaml` | Secret + ConfigMap | JWT/密码/Token 注入 |

```bash
# 1. 构建镜像并推送到仓库
docker build --build-arg TARGET=wiki-api -t registry/wiki-api:latest .
docker build --build-arg TARGET=rag-worker -t registry/rag-worker:latest .
docker build --build-arg TARGET=mcp-server -t registry/mcp-server:latest .
docker build -f deployments/Dockerfile.web -t registry/wiki-frontend:latest .

# 2. 生成 migration ConfigMap
bash deployments/chart/hack/generate-migrations-cm.sh

# 3. 部署
helm install wiki ./deployments/chart/wiki \
  --set postgresql.auth.password="prod-pg-pass" \
  --set config.jwtSecret="prod-jwt-secret-here" \
  --set config.internalServiceToken="prod-internal-token" \
  --set frontend.ingress.host="wiki.example.com" \
  --set frontend.ingress.tls.enabled=true \
  --set image.registry="registry/"
```

**Helm 特性：**
- **滚动升级**：所有应用 Deployment 配置 `maxUnavailable: 0`，零宕机升级
- **健康检查**：livenessProbe + readinessProbe（HTTP / TCP）
- **HPA**：wiki-api 默认 2-10 副本，CPU > 70% 自动扩容
- **PVC**：每个有状态组件独立 PVC，`persistence.defaultStorageClass` 全局可配
- **Ingress**：前端 nginx 可选 Ingress + TLS（cert-manager 自动证书）

> Helm Chart 中的 `migrations-cm.yaml` 由脚本自动生成：`bash deployments/chart/hack/generate-migrations-cm.sh`。每次 SQL 迁移变更后需重新生成。

### 生产部署注意事项

1. **必改密钥**：`JWT_SECRET`（≥32 字符）、`POSTGRES_PASSWORD`、`INTERNAL_SERVICE_TOKEN`
2. **资源分配**：TEI 内存约 2GB+，Qdrant 视数据量调整；宿主端口避免冲突
3. **模型选择**：默认 `all-MiniLM-L6-v2`（dim 384）轻量；生产推荐 `Qwen/Qwen3-Embedding-0.6B`（≥4GB 内存）
4. **中文搜索**：标准 postgres:16 无 zhparser，FTS 使用 `simple` 配置；如需中文分词，用 `pgvector/pgvector:pg16` 或自定义镜像
5. **TLS**：前端 nginx.conf 已内置 TLS 配置模板（注释启用）；K8s Ingress 可配合 cert-manager

### 故障排查

```bash
# 查看某服务日志
docker compose -f deployments/docker-compose.yml logs wiki-api
docker compose -f deployments/docker-compose.yml logs rag-worker
docker compose -f deployments/docker-compose.yml logs tei

# 检查容器健康状态
docker compose -f deployments/docker-compose.yml ps

# 重置全部数据（⚠ 清空）
docker compose -f deployments/docker-compose.yml down -v

# 单独重启某服务
docker compose -f deployments/docker-compose.yml restart rag-worker
```

## 测试

```bash
# 单元测试（无需 DB）
go test ./...

# 集成测试（需可连通的 Postgres，含 9 套迁移）
DATABASE_URL="host=/tmp port=5433 user=wiki dbname=wiki sslmode=disable" \
  go test -tags=integration ./test/integration/...
```

## RBAC 决策引擎（AC-7 核心）

`internal/platform/rbac/engine.go` 实现纯逻辑决策，优先级：
**显式拒绝 > 显式允许 > 继承 > 默认拒绝**。
- `admin` 蕴含 read+write+admin；`write` 蕴含 read。
- 引擎按 target 链（文档→目录→工作区）从具体到宽泛逐级判定，任一级显式拒绝即拒绝。
- 权限变更通过事件触发 RAG 侧 chunk `visible_to` 重算（本仓发布事件，重算在 YS-8）。
- 无权限文档不在目录树与检索结果中（存在性不泄露）。

## 安全

- 全部 SQL 参数化（pgx），FTS 配置名严格白名单（`[a-zA-Z0-9_]`）防注入。
- 审计日志追加写（`audit_logs` 分区表，仅 INSERT）。
- JWT 鉴权 + 速率限制（按用户/域）。
- 默认不出网；密钥经环境变量注入，不硬编码。
