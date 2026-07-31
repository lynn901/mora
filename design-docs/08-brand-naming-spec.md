# 品牌命名规范一页纸：wiki → mora

> 状态：**基准文档（阶段1前置交付物）** · 所有更名任务（YS-44 ~ YS-51）以此为准
> 来源：YS-41 产品经理决策（见 YS-41 评论 `b8fd8ee1` / `ff05d99c`）
> 适用范围：mora 仓库全链路——前端 / 后端 Go / 配置 / 数据库 / 向量库 / 部署 / 文档
> 盘点基线：150 文件、944 处 "wiki" 引用

本规范解决一个问题：**各团队按什么规则把 `wiki` 改成 `mora`，才不会大小写不一、用词不一、误改通用功能词**。文末「各层映射对照表」给出逐项新旧对照与责任子任务，可直接照搬执行。

---

## 1. 品牌词规范

| 用途 | 形态 | 示例 |
|---|---|---|
| **展示文本**（UI 标题、登录页、欢迎语、README、OpenAPI 标题、Grafana 面板名、脚本注释） | `Mora`（首字母大写） | `Sign in to Mora`、`Welcome to Mora`、`# Mora 后端核心` |
| **代码标识符 / 路径 / 配置键 / 服务名 / DB / 集合名** | `mora`（全小写） | `mora-api`、`components/mora/`、`MORA_API_PORT`、`mora` 库、`mora_chunks_*` |

一句话：**给人看的大写首字母 `Mora`，给机器看的小写 `mora`。**

> 说明：仓库名已是 `mora`（`github.com/lynn901/mora`），Go module path 须与之对齐（见 §4-C）。

---

## 2. 大小写规则

按标识符类型套用对应命名约定，**品牌词 `mora` 作为普通词根参与**，不搞特殊大小写：

| 类型 | 约定 | 旧 → 新 示例 |
|---|---|---|
| 类型 / 类名 / 接口 | PascalCase | `WikiState` → `MoraState`；`WikiCollabProvider` → `MoraCollabProvider`；`WikiLayout` → `MoraLayout` |
| 变量 / 函数 / Store hook | camelCase | `useWikiStore` → `useMoraStore`；`wikiToken` → `moraToken` |
| 常量 / 环境变量 | SCREAMING_SNAKE_CASE | `WIKI_API_PORT` → `MORA_API_PORT`；`WIKI_FRONTEND_PORT` → `MORA_FRONTEND_PORT`；`WIKI_API_URL` → `MORA_API_URL` |
| 路径 / 包名 / 目录 / 服务名 / 镜像名 / Helm Chart 名 | lowercase（连字符分隔） | `cmd/wiki-api/` → `cmd/mora-api/`；`components/wiki/` → `components/mora/`；服务 `wiki-api` → `mora-api`；镜像 `registry/wiki-api` → `registry/mora-api`；Chart `wiki` → `mora` |
| Go module path | lowercase | `github.com/wiki/wiki-backend` → `github.com/lynn901/mora` |
| DB 名 / 用户 / 角色 | lowercase | 库 `wiki`→`mora`；用户 `wiki`→`mora`；role `wiki_app`→`mora_app` |
| Qdrant collection 前缀 | lowercase + 下划线 | `wiki_chunks_` → `mora_chunks_` |
| localStorage key | lowercase + 下划线 | `wiki_token` → `mora_token` |
| nginx upstream 名 | lowercase + 下划线 | `wiki_api` → `mora_api` |
| COMPOSE_PROJECT | lowercase | `wiki` → `mora` |

**判别口诀**：先看标识符属于哪一类（类名？变量？常量？路径？），套该类的约定；品牌词只是词根，照常变形。

---

## 3. 品牌词 vs 通用功能词界定（重要）

### 规则

- **只替换「品牌级 Wiki」**：指代本产品 / 产品后端 / 产品 API 的 `Wiki`，一律改为 `Mora`/`mora`。
- **通用功能词不保留 `wiki` 字样**：把 `wiki` 当通用名词（指"wiki 形态的页面 / 双链 / 知识库"）的用例，统一改为**产品内术语**（推荐：`知识库` / `知识库页面` / `文档`），**不再保留 wiki**。这样改名后产品内无任何 wiki 字样，避免读者混淆"是品牌还是功能"。

### 待判定项清单（盘点结论）

| # | 位置 | 原文 | 性质 | 处理结论 | 责任任务 |
|---|---|---|---|---|---|
| 1 | `web/src/api/mock.ts:56` | `This is a **collaborative** wiki with...` | 通用功能词（指 wiki 形态知识库） | 改为 `协作式知识库`，不保留 wiki | YS-44（前端文案） |
| 2 | `web/src/api/mock.ts:56` | `Welcome to the Wiki platform` / `Hello, Wiki!` | 品牌级 | `Welcome to the Mora platform` / `Hello, Mora!` | YS-44 |
| 3 | `web/src/api/mock.ts:94` | `This is a collaborative wiki.` | 通用功能词 | `This is a collaborative knowledge base.` | YS-44 |
| 4 | `README.md:3` | `协同 Wiki 平台后端` | 品牌级（Wiki=产品名） | `Mora 协同知识库平台后端` | YS-45（后端文档） |
| 5 | `design-docs/01~07` 多处 | `Wiki API` / `Wiki 后端` / `Wiki backend` | 品牌级（指本产品后端组件） | `Mora API` / `Mora 后端` / `Mora backend` | YS-45 |
| 6 | `design-docs/02-system-architecture.md:27` 等 | 架构图中 `Wiki API 服务` | 品牌级 | `Mora API 服务` | YS-45 |
| 7 | `web/src/components/wiki/WikiLayout.tsx:149` | 文档标题占位 `"Wiki"` | 品牌级 | `"Mora"` | YS-44 |
| 8 | `web/src/components/wiki/WikiLayout.tsx:178` | `Welcome to Wiki` | 品牌级 | `Welcome to Mora` | YS-44 |
| 9 | `migrations/010_seed_demo.up.sql:46` | MCP dev token 前缀 `wki_dev_a1b2` | 标识符（`wki` 系 `wiki` 缩写） | 改为 `mora_dev_` 前缀；属破坏性变更（旧 token 失效），归入阶段3数据层；仅 dev/demo，影响可控 | YS-49 |
| 10 | `internal/platform/config/config.go:96,136` | `MCP_SERVER_NAME` 默认值 `wiki-mcp` | 品牌级（服务名） | 默认值改 `mora-mcp` | YS-47（后端代码标识符） |
| 11 | Helm `Chart.yaml` keywords `[wiki, knowledge-base, collaboration]` | 品牌级元数据 | keywords 改 `[mora, knowledge-base, collaboration]` | YS-48 |
| 12 | 仓库内"wiki 页面 / wiki 双链"类用法 | 全仓库扫描未发现"wiki 页面""wiki 双链"作为固定术语的用例（功能词用法仅见于 mock.ts 第 1/3 项） | — | 无遗留，按 §3 规则处理上述即可 | — |

> 执行原则：遇到拿不准的 `wiki`，先判断它指代"本产品"（品牌级→Mora）还是"wiki 这种东西"（功能词→产品术语）。拿不准的在对应子任务评论里 @项目管理助手 判定，不要自行猜测。

---

## 4. 各层映射对照表

> 责任任务列对应 YS-44 ~ YS-51。阶段：1=文案/文档（低风险）·2=代码标识符重构（中）·3=配置/DB/向量/基础设施（高，破坏性，须原子切换）。

### 4-A 前端用户可见文案（阶段1 · YS-44）

| 旧 | 新 | 位置 |
|---|---|---|
| `Wiki - Collaborative Knowledge Base` | `Mora - Collaborative Knowledge Base` | `web/index.html:7` `<title>` |
| `Sign in to Wiki` | `Sign in to Mora` | `web/src/components/auth/LoginPage.tsx:35` |
| `Welcome to Wiki` | `Welcome to Mora` | `web/src/components/wiki/WikiLayout.tsx:178` |
| 文档标题占位 `"Wiki"` | `"Mora"` | `web/src/components/wiki/WikiLayout.tsx:149` |
| mock 演示文案中品牌级 Wiki | Mora | `web/src/api/mock.ts:56,94`（功能词见 §3-1/3） |

### 4-B 前端代码标识符（阶段2 · YS-46）

| 旧 | 新 | 位置 |
|---|---|---|
| `useWikiStore` | `useMoraStore` | `web/src/stores/wiki.ts:29` |
| `WikiState` | `MoraState` | `web/src/stores/wiki.ts:6` |
| `web/src/stores/wiki.ts` | `web/src/stores/mora.ts` | 文件重命名 + 全仓 import 同步 |
| `WikiCollabProvider` | `MoraCollabProvider` | `web/src/lib/collab-provider.ts:73` |
| `WikiLayout` | `MoraLayout` | `web/src/components/wiki/WikiLayout.tsx:20` |
| `web/src/components/wiki/` | `web/src/components/mora/` | 目录重命名 + import 同步 |
| localStorage key `wiki_token` | `mora_token` | `web/src/api/client.ts:17,21,25`（⚠ 旧用户登录态失效，发布说明标注） |
| mock 邮箱域 `@wiki.dev` | `@mora.dev` | `web/src/api/mock.ts:17-19`（标识性数据） |

### 4-C 后端 Go 代码标识符（阶段2 · YS-47）

| 旧 | 新 | 位置 |
|---|---|---|
| Go module `github.com/wiki/wiki-backend` | `github.com/lynn901/mora` | `go.mod:1`（与仓库名一致） + 全仓 import 同步 |
| `cmd/wiki-api/` | `cmd/mora-api/` | 目录 + `deployments/Dockerfile` `ARG TARGET=wiki-api`→`mora-api` |
| `internal/module/wiki/` | `internal/module/mora/` | 目录 + import 同步 |
| `internal/module/mcp/wikiclient/` | `internal/module/mcp/moraclient/` | 目录 + import 同步 |
| `MCP_SERVER_NAME` 默认 `wiki-mcp` | `mora-mcp` | `internal/platform/config/config.go:96,136` |
| 注释中 `Wiki backend` / `wiki-api` 等 | `Mora backend` / `mora-api` | 代码注释（品牌级） |

> 注：`config.go` 读的环境变量键名 `WIKI_API_URL` 的**实际切换属阶段3**（见 4-D）；本阶段只改代码内默认值/注释。

### 4-D 配置 / 环境变量（阶段3 · YS-48）

| 旧 | 新 | 位置 |
|---|---|---|
| `WIKI_API_PORT` | `MORA_API_PORT` | `.env.example:6`、`docker-compose.yml:119`、`README.md:106` |
| `WIKI_FRONTEND_PORT` | `MORA_FRONTEND_PORT` | `.env.example:8`、`docker-compose.yml:101`、`README.md:108` |
| `WIKI_API_URL`（mcp-server 读） | `MORA_API_URL` | `docker-compose.yml:194`、`config.go:91,154`（代码读取键名） |
| `INTERNAL_SERVICE_TOKEN` 默认 `wiki-internal-token` | `mora-internal-token` | `docker-compose.yml:135,195`、`.env.example` |
| `POSTGRES_PASSWORD` 默认 `wiki` | `mora` | `docker-compose.yml:27,91,126,160,192`、`.env.example` |
| `COMPOSE_PROJECT = wiki` | `mora` | `Makefile:15`、`docker-compose.yml:272`、`backup.sh:14`、`restore.sh:16`、`export.sh:13` |

> ⚠ `COMPOSE_PROJECT` 改名会改变数据卷命名：`wiki_pg_data` → `mora_pg_data`（卷名 `pg_data`，见 `docker-compose.yml:297`）。旧卷数据失联，必须与数据层迁移同窗口原子切换，迁移脚本含卷重命名/重新挂载步骤（YS-50）。

### 4-E 数据库（阶段3 · YS-49）

| 旧 | 新 | 位置 |
|---|---|---|
| DB 名 `wiki` | `mora` | `docker-compose.yml:25`、`run-migrations.sh:11`、`DATABASE_URL` 连接串 |
| DB 用户 `wiki` | `mora` | `docker-compose.yml:26`、`run-migrations.sh:10`、`pg_dump -U wiki` 等 |
| DB role `wiki_app` | `mora_app` | `migrations/006_audit.up.sql:31`（注释引用）、role 定义迁移 |
| `DATABASE_URL` `postgres://wiki:...@.../wiki` | `postgres://mora:...@.../mora` | `docker-compose.yml:126,160,192`、`README.md:267` |
| 种子邮箱 `admin@wiki.local` | `admin@mora.local` | `migrations/001_users.up.sql:66`、`migrations/010_seed_demo.up.sql:9,14,22,47`、`README.md:80` |
| 工作区 slug `eng-wiki` | `eng-mora` | `migrations/010_seed_demo.up.sql:13` |
| pg_isready `-U wiki -d wiki` | `-U mora -d mora` | `docker-compose.yml:29` |
| 备份/恢复脚本 `pg_dump -U wiki -d wiki`、`psql -U wiki -d wiki` | `-U mora -d mora` | `backup.sh:22`、`restore.sh:45,49,99`、`export.sh:28,86` |
| 导出产物名 `wiki_pg_dump.sql`、`wiki-export-*` | `mora_pg_dump.sql`、`mora-export-*` | `backup.sh:23-25,66`、`export.sh:14,29`、`docker-compose.yml:291` |

### 4-F 向量库（阶段3 · YS-49）

| 旧 | 新 | 位置 |
|---|---|---|
| Qdrant collection 前缀 `wiki_chunks_` | `mora_chunks_` | `internal/domain/chunk.go:41`（`CollectionName()`） |
| `CollectionPrefix` 默认 `"wiki_chunks"` | `"mora_chunks"` | `internal/module/rag/pipeline/config.go:16,29` |
| `migrations/qdrant_collections_init.sql` 注释/示例 `wiki_chunks_*` | `mora_chunks_*` | `migrations/qdrant_collections_init.sql:6` |

> 📌 PM 建议（已采纳）：把 collection 前缀做成**可配置项**（默认 `mora_chunks_`，由 env 注入），未来更名零成本。归入 YS-49 范围。
> ⚠ 改前缀会使已有向量集合失联，迁移脚本须含 rename collection 或重建索引步骤（YS-50）。

### 4-G 基础设施 / 部署（阶段3 · YS-48）

| 旧 | 新 | 位置 |
|---|---|---|
| Docker 服务名 `wiki-api` | `mora-api` | `docker-compose.yml:113`、`nginx.conf:6`、`docker-compose.yml:103,188`（depends_on） |
| nginx upstream `wiki_api` | `mora_api` | `deployments/nginx.conf:5,49,60` |
| nginx upstream 目标 `wiki-api:8080` | `mora-api:8080` | `deployments/nginx.conf:6` |
| 镜像 `registry/wiki-api` / `registry/wiki-frontend` | `registry/mora-api` / `registry/mora-frontend` | `README.md:207,210` |
| `--build-arg TARGET=wiki-api` | `mora-api` | `deployments/Dockerfile:8`、`README.md:207` |
| Helm Chart 目录 `deployments/chart/wiki/` | `deployments/chart/mora/` | 目录重命名 |
| `Chart.yaml` `name: wiki` | `name: mora` | `deployments/chart/wiki/Chart.yaml:3` |
| Chart keywords `[wiki, knowledge-base, collaboration]` | `[mora, knowledge-base, collaboration]` | `Chart.yaml` keywords |
| Helm 模板函数 `wiki.name`/`wiki.fullname`/`wiki.labels`/`wiki.selectorLabels`/`wiki.image` | `mora.name`/`mora.fullname`/`mora.labels`/`mora.selectorLabels`/`mora.image` | `templates/_helpers.tpl` 全量 + 所有模板引用 |
| 模板文件 `templates/wiki-api.yaml` | `templates/mora-api.yaml` | 文件重命名 + `helm install` 文档同步 |
| `helm install wiki ./deployments/chart/wiki` | `helm install mora ./deployments/chart/mora` | `README.md:216` |

### 4-H 平台元数据（非代码仓库，由项目管理助手在平台侧协调）

| 旧 | 新 | 备注 |
|---|---|---|
| 小队名「wiki 小队」 | 「mora 小队」 | Multica 平台侧 |
| Agent 名「Wiki知识库产品经理」等 | 「Mora知识库产品经理」 | Multica 平台侧 |
| Multica 项目名「wiki」 | 「mora」 | Multica 平台侧 |

> 4-H 不在代码仓库内，由项目管理助手在 Multica 平台协调更名，不阻塞代码侧各阶段。

---

## 5. 实施节奏与依赖（决策4落地）

| 阶段 | 风险 | 范围 | 责任任务 | 依赖 |
|---|---|---|---|---|
| **阶段1（P0）** | 低 | A 类用户可见文案 + 文档/README/OpenAPI/Grafana 注释 | YS-44（前端）、YS-45（后端文档） | 无硬前置，参照本规范 |
| **阶段2（P1）** | 中 | B/C 类代码标识符重构（行为不变，回归保障） | YS-46（前端）、YS-47（后端） | 建议阶段1完成后；前后端可并行 |
| **阶段3（P2）** | 高·破坏性 | D/E/F/G 配置/DB/向量/基础设施 + 迁移脚本 + 端到端验证 | YS-48、YS-49、YS-50、YS-51 | **必须原子切换**，同一维护窗口上线 |

**阶段3原子切换要求**：「读 `MORA_*` 配置 / 连 `mora` 库 / `mora_chunks_*` 集合的代码」须与 env / DB / 卷 / Helm 改名**在同一维护窗口一起生效**，避免"代码读 `MORA_*`、env 仍是 `WIKI_*`"的断裂。阶段2代码可先合并，但读新配置键的代码生效点须与 infra 改名同步。

**干净切换，不保留旧别名**：现有部署须同步更新 `.env`、卷名、DB 名，否则服务起不来。Changelog / 部署文档须醒目标注破坏性变更。

---

## 6. 验收口径（供各任务 DoD 引用）

- **品牌词**：全仓库无品牌级 `wiki`/`Wiki` 残留；通用功能词 `wiki` 已改为产品术语、无残留。
- **大小写**：类名 PascalCase、变量 camelCase、环境变量 SCREAMING_SNAKE、路径/服务名 lowercase，品牌词 `mora` 作普通词根参与变形。
- **映射对照表**：4-A~4-G 各项新旧一一落地，脚本扫描无残留 wiki 标识符（排除已判定保留项）。
- **破坏性变更**：迁移脚本 + 回退 runbook + 数据备份步骤齐备（YS-50），端到端验证通过（YS-51）。

---

## 附：决策依据

本规范基于 YS-41 评论区产品经理决策（评论 `ff05d99c`）：
1. 品牌词规范：展示 `Mora`、代码 `mora` ✓
2. 替换范围：全量替换，一次干净切换 ✓
3. 破坏性迁移：COMPOSE_PROJECT 卷名陷阱、向量前缀可配置、干净切换无别名 ✓
4. 实施节奏：分 3 阶段，阶段3 原子切换 ✓

如对某映射项有歧义，在对应子任务评论 @项目管理助手 判定；本规范为活文档，后续发现的新待判定项追加至 §3 清单。
