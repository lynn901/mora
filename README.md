# Wiki 后端核心（YS-6）

协同 Wiki 平台后端，Go 模块化单体实现。依据 Stage 1 交付的 7 份设计文档
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

## 运行

一键拉起全部后端组件（3 应用 + 4 基础设施 + 迁移 init 容器）：

```bash
# 全部 7 个服务 + 迁移：postgres/valkey/qdrant/tei + wiki-api/rag-worker/mcp-server
docker compose -f deployments/docker-compose.yml up -d

# 迁移由 migrate init 容器自动执行（001-010，幂等，schema_migrations 记录）
# 健康检查通过后即可访问：
#   wiki-api   http://localhost:8080  (/healthz /ready)
#   mcp-server http://localhost:8081  (/mcp/health)

# 联调种子（迁移 010）已注入：admin@wiki.local/admin123、演示工作区、
#   激活的 embedding 模型、MCP dev token (wki_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4)

# 可选 Ollama（替代 TEI 作为 Embedding Provider）：
docker compose -f deployments/docker-compose.yml --profile ollama up -d
```

> TEI 首次启动需从 HuggingFace 下载 embedding 模型（约 1.2GB，缓存于 `tei_cache` 卷）。
> 完全离线环境请预置模型或改用 Ollama 本地模型。

本地直接运行（不走容器）：

```bash
DATABASE_URL=postgres://wiki:wiki@localhost:5432/wiki go run ./cmd/wiki-api
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
