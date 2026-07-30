# E2E 验证套件（YS-16）

黑盒端到端测试，通过 HTTP 驱动运行中的平台（wiki-api + mcp-server + rag-worker +
PG/Valkey/Qdrant/TEI），覆盖 PRD（YS-4）AC-1~19、核心闭环（§5.1）、RBAC 跨层一致性与
§7 边界场景。用例/脚本先行产出，待 YS-12 基础设施就绪后由 YS-10 一键执行。

覆盖矩阵见 [`VERIFICATION_MATRIX.md`](./VERIFICATION_MATRIX.md)。

## 设计说明

- **黑盒 HTTP**：仅通过 REST/JSON-RPC 驱动运行态服务，不导入 `internal/` 包，不直接走仓储层
  （区别于 `test/integration/` 的白盒 DB 集成测试）。复用仓库既有 testify + pgx + bcrypt 依赖，**未引入新依赖**。
- **build tag `e2e` + env 门控**：未设 `E2E_BASE_URL` 时整个包跳过（`TestMain` 退出 0），与
  `test/integration` 的 `integration` tag + `DATABASE_URL` 门控同构。
- **夹具隔离**：每个用例创建带唯一标记（`E2E-`/随机后缀）的工作区/目录/文档，串行执行；
  非管理员用户与自定义 Token 经 `DATABASE_URL` 播种，套件结束时清理。

## 运行

### 1. 拉起全组件栈（YS-12）

```bash
# 全栈 + 额外暴露 postgres:5432 供 E2E 夹具播种
docker compose -f deployments/docker-compose.yml \
               -f tests/e2e/docker-compose.e2e.override.yml up -d

# 健康就绪后：
#   wiki-api   http://localhost:8990  (容器内 :8080)
#   mcp-server http://localhost:8081
```

> `docker-compose.e2e.override.yml` 仅给 `postgres` 加 `ports: ["5432:5432"]`，便于从宿主机
> 播种非管理员用户/Token。生产部署不应暴露该端口。

### 2. 执行 E2E

```bash
E2E_BASE_URL=http://localhost:8990 \
E2E_MCP_URL=http://localhost:8081 \
DATABASE_URL=postgres://wiki:wiki@localhost:5432/wiki \
INTERNAL_SERVICE_TOKEN=wiki-internal-token \
go test -tags=e2e -v ./tests/e2e/...
```

只跑某条用例：

```bash
go test -tags=e2e -v -run TestCoreClosedLoop ./tests/e2e/...
```

## 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `E2E_BASE_URL` | http://localhost:8990 | wiki-api 基址；未设则整包跳过 |
| `E2E_MCP_URL` | http://localhost:8081 | mcp-server 基址 |
| `DATABASE_URL` | （空） | PG DSN；未设则需 DB 夹具的用例 skip |
| `INTERNAL_SERVICE_TOKEN` | wiki-internal-token | 可信内部调用凭证（MCP→wiki 透传身份用） |
| `E2E_ADMIN_EMAIL` | admin@wiki.local | 管理员邮箱（迁移 010 种子） |
| `E2E_ADMIN_PASSWORD` | admin123 | 管理员密码 |
| `E2E_DEV_TOKEN` | wki_dev_a1b2c3d4... | MCP dev token（readwrite，绑定 admin） |
| `E2E_INDEX_TIMEOUT` | 120s | index_status 到达 indexed 的轮询窗口 |

## 种子数据（迁移 010）

- 管理员 `admin@wiki.local` / `admin123`（JWT 登录，isAdmin）
- 演示工作区 `11111111-...`（slug `eng-wiki`）
- 激活 embedding 模型：tei / Qwen/Qwen3-Embedding-0.6B / dim 1024
- MCP dev token `wki_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4`（readwrite）

E2E 套件额外播种（需 `DATABASE_URL`，套件结束清理）：
- `e2e_alice@wiki.local`（alice123）、`e2e_bob@wiki.local`（bob123）—— 非管理员用户
- alice：readwrite / readonly Token；bob：readonly Token；外加过期 Token、可撤销 Token

## 测试组织

| 文件 | 覆盖 |
|------|------|
| `helpers_test.go` | 配置/客户端/鉴权/DB 夹具/MCP JSON-RPC 助手 |
| `core_closed_loop_test.go` | 核心闭环 5 步（PRD §5.1） |
| `rbac_cross_layer_test.go` | RBAC 跨层一致性 + 存在性不泄露 |
| `mcp_protocol_test.go` | AC-14~19（initialize/resources/tools/写草稿/鉴权审计/作用域） |
| `ac_wiki_test.go` | AC-1/4/6/7/8 |
| `ac_rag_test.go` | AC-9~13 |
| `boundary_test.go` | PRD §7 边界与异常 |

## 已知实现缺口（执行前排查）

以下为代码与设计契约的已知偏差；若对应用例失败，先确认是缺口而非脚本问题。这些缺口
属 YS-10「mock→真实集成」与 YS-12 联调范围，本套件已规避或以 skip 标注：

1. `GET /api/v1/documents/:id/versions` 返回桩（stub）——AC-6 改用 mounted 的
   `diff`+`rollback`+`version_no` 路径验证。
2. `/api/v1/admin/embedding-models*` 与 `GET /documents/:id/index-status` 未在 wiki-api
   注册（handler 仅在 rag e2e 测试 mux 中挂载）——AC-11 模型热切换/重建 skip；连通性通过
   `index_status=indexed` 间接验证；index_status 经 `GET /documents/:id` 响应字段读取。
3. `GET /api/v1/workspaces/:id/tags` 未注册——AC-8 标签筛选用 `directory_id`+`created_by` 维度。
4. wiki-api `GET /documents/:id` 忽略 `format`/`version` 查询参数，始终返回 Block 数组。
5. MCP 401 返回 JSON-RPC 错误体（`code:-32001`），非 wiki-api `{code,message}` 信封。
6. MCP `wikiclient` 解码信封字段为 `msg` 而 wiki-api 实际发 `message`（上游错误信息被静默丢弃）。
7. MCP `create_draft`/`update_document` 经 `wikiclient` 发 `content` 为字符串，wiki-api 期望 `markdown`/`[]Block` → MCP 写工具当前 400。AC-17 MCP 路径 skip-with-note（草稿态在 wiki 层验证）；MCP 越权用例改断言安全结果，精确 403 由 `TestBoundary_WikiWriteRBACDenied` 在 wiki 层验证。

## infra 依赖场景（提供手动步骤，自动 skip）

- **模型不可用降级**（`TestBoundary_ModelUnavailableDegradation`）：停 `tei` 容器，建文档确认
  index_status 停留 processing/failed，而 `/search`（FTS）与 RAG BM25 路径仍返回既有文档。
- **限流**（`TestBoundary_RateLimited`）：调低 `MCP_RATE_LIMIT_READ`，突发 `tools/call`，预期 429 + Retry-After。
- **流水线失败重试×3/死信**：由 `rag-worker` 单测覆盖；E2E 断 TEI 验证幂等见 `TestAC13`。
