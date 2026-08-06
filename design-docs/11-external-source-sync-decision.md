# 外部源同步技术选型决策书

> 文档版本：v1.0 ｜ 产出人：Mora项目架构师 ｜ 对应任务：YS-70《分析 WeKnora 功能并制定 Mora 整体规划》§1.2 / 阶段二 P1
> 父决策：01-tech-selection-decision.md（自研路线 + 全组件宽松 License 基线）、10-multi-format-parsing-decision.md（#1，全量导入复用 Parser）
> 产品基线：YS-70 PM 初版规划 §1.2「外部数据源同步」（飞书/语雀优先，全量一次性导入，增量映射为新版本，权限映射避免无主文档绕过存在性不泄露）
> 评审状态：**草案 v1.0**——选型论证 + 同步映射模型 + 权限归属策略 + 增量对账机制；门控与被低估改动已显式标注

---

## 1. 决策背景与范围

### 1.1 背景

PM 规划将「外部源同步」列 P1，定位为**部署期/迁移期一次性或低频导入**（非持续出网拉取），限定连接企业内部自建的飞书/语雀私有部署实例，规避与「默认不出网」原则的冲突（`design-docs/07 §6`）。目标是把企业存量知识迁移进 Mora，复用 #1 已定的 Parser → Block → 版本链路。

### 1.2 P1 范围

- **飞书** + **语雀** 全量一次性导入连接器（MVP）；Notion 次之；RSS 列可选。
- 全量导入：拉取源文档 → 复用 #1 Parser → Block → 落目标 workspace/directory。
- 增量同步：源端变更映射为 Mora 文档新版本（可 Diff）。
- 同步任务纳入审计日志（复用现有 `AuditRepo.Append`）。
- 默认配置不出网：需用户显式配置源地址 + 凭据。

### 1.3 决策原则（继承 01 §1.2 + 10 §1.2）

私有化优先、License 合规、复用既有链路（Parser #1 / 版本链 / RBAC / 审计）、权限映射不破坏存在性不泄露、分步推进。

---

## 2. 候选方案评估

### 2.1 连接器实现方式

| 候选 | License / 接入方式 | 选型定位 |
|---|---|---|
| **飞书开放平台 SDK（官方）** | 官方 REST API + app_id/app_secret；Go 可直调 HTTP，无需三方 SDK | **选用**：私有部署飞书支持 open-api |
| **语雀官方 API** | 官方 REST API + token | **选用**：Go 直调 HTTP |
| 三方 SDK（如 `larksuite/oapi-sdk-go`） | Apache-2.0 | **可选**：若手写 HTTP 样板过重再引入；P0 先直调 |
| RSS | `golang.org/x/net/html` + 现有 Parser | 可选增强，P1 不强制 |

**结论**：连接器层走「官方 API + Go 直调 HTTP」为主，避免引入三方 SDK（larksuite Apache-2.0 本可引入，但 P1 MVP 样板量不大，直调更可控、依赖更少）。License 全合规（无传染）。

### 2.2 全量导入路径（复用 #1）

源文档导出格式 → #1 Parser → `[]domain.Block` → 复用 `DocumentService.Create`（`service/document.go:46`）：
- `Create` 已做 RBAC 写校验（admin bypass 或 workspace write）、`CreatedBy = auth.UserID`、快照 version 1、发布 `EventCreate`（触发 RAG 向量化）；
- `documents.content` JSONB 承接 Block（含 #1 §8 的 `BlockTable`）；`content_text` 由 `BlockArray.PlainText()` 填充；
- 同步文档落目标 directory（`directory_id`），目录树走 ltree `path`（`002_workspaces`）。

**无需改动 DocumentService.Create 主体**——连接器构造 `AuthContext{UserID: <sync 归属用户>, IsAdmin: <视策略>}` 调用即可。此路径最短，符合 PM「仅依赖 #1」判断。

### 2.3 增量对账映射表（`document_sources`）

`documents` 现无外部源映射列（`003_documents.up.sql`），需新增独立映射表（不污染 `documents` 主表）：

```sql
-- migrations/011_sync.up.sql（新增，P1）
CREATE TABLE sync_connectors (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    source_type   VARCHAR(20) NOT NULL,        -- feishu/yuque/notion
    base_url      TEXT NOT NULL,              -- 私有部署实例地址（显式配置，不出网默认）
    credentials   JSONB NOT NULL DEFAULT '{}', -- 加密存 app_secret/token（见 §5）
    target_directory_id UUID REFERENCES directories(id) ON DELETE SET NULL,
    owner_user_id UUID NOT NULL REFERENCES users(id), -- 同步文档归属（见 §4）
    default_role  VARCHAR(20) NOT NULL DEFAULT 'read', -- 落地 directory 默认角色
    sync_mode     VARCHAR(20) NOT NULL DEFAULT 'full', -- full/incremental
    last_synced_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, source_type, base_url)
);

CREATE TABLE document_sources (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    connector_id  UUID NOT NULL REFERENCES sync_connectors(id) ON DELETE CASCADE,
    external_id   TEXT NOT NULL,              -- 源端稳定 ID
    source_url    TEXT,
    content_hash  TEXT,                        -- 内容指纹，增量比对
    last_synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(connector_id, external_id)
);

CREATE INDEX idx_doc_sources_doc ON document_sources(document_id);
CREATE INDEX idx_doc_sources_conn_ext ON document_sources(connector_id, external_id);
```

- 增量逻辑：重同步时按 `(connector_id, external_id)` 匹配；比对 `content_hash`，变更则走 `DocumentService.Update`（`document.go:~70`）追加 `document_versions`（`UNIQUE(document_id, version_no)` 天然承接，`author_id = owner_user_id`），可 Diff。
- 删除检测：源端已删则在 `sync_runs` 记录，**不在 Mora 侧硬删**（不可改写版本模型），可标记 `status=archived` 或留审计痕迹，由人决定。

### 2.4 同步运行记录

```sql
CREATE TABLE sync_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    connector_id  UUID NOT NULL REFERENCES sync_connectors(id) ON DELETE CASCADE,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    status        VARCHAR(20) NOT NULL,       -- running/succeeded/failed/partial
    total         INTEGER DEFAULT 0,
    succeeded     INTEGER DEFAULT 0,
    failed        INTEGER DEFAULT 0,
    error_detail  JSONB DEFAULT '{}'
);
CREATE INDEX idx_sync_runs_conn ON sync_runs(connector_id, started_at DESC);
```

---

## 3. 决策结论

| 项 | 选型 | 说明 |
|---|---|---|
| 飞书/语雀连接器 | 官方 API + Go 直调 HTTP | 无三方 SDK 依赖，私有部署实例显式配置 |
| 全量导入 | 复用 #1 Parser + `DocumentService.Create` | 零主体改动 |
| 增量对账 | 新增 `sync_connectors`/`document_sources`/`sync_runs` 三表 | 独立映射表，不污染 `documents` 主表 |
| 版本承接 | 复用 `document_versions`（append-only） | 增量变更 = 新版本 |
| 审计 | 复用 `AuditRepo.Append`（`repos.go:76`） | 同步行为入 `audit_logs` |
| 权限归属 | **专用同步用户**（`users` 行 + RBAC grant） | 见 §4（关键决策） |

---

## 4. 权限映射与服务账号归属策略（关键决策 + 被低估改动）

> ⚠️ 架构师复核 PM「权限映射避免无主文档绕过存在性不泄露」时发现一处**被低估的改动**：`service_accounts` 表存在但不能作为 RBAC subject，原「服务账号归属」策略需调整。

### 4.1 现状核实（代码依据）

- `service_accounts` 表存在（`001_users.up.sql`），但设计**仅供 API Token 绑定**（`008_mcp.api_tokens.identity_type='service_account'` + `identity_id → service_accounts.id`）。
- RBAC 域模型 `SubjectType` 只有 `SubjectUser`/`SubjectGroup`（`domain/rbac.go:49-50`），**无 `SubjectServiceAccount`**；`permissions` 表 `subject_type ∈ {user, group}`，`created_by` 外键 `users(id)`。
- RBAC 引擎 `Engine.Check` 签名是 `Check(ctx, subject uuid.UUID, groupIDs []uuid.UUID, ...)`（`platform/rbac/engine.go`），`GrantsFor` SQL 只查 `subject_type='user' OR subject_type='group'`（`infra/postgres/rbac.go:101-102`）——**无法为 service_account 授权**。
- `DocumentService.AuthContext` 只有 `UserID`+`Groups`+`IsAdmin`（`document.go:36`），`Create` 设 `CreatedBy = auth.UserID`（`users.id` 外键，`003:16`）——**不能写 service_accounts.id**。

**结论**：`service_accounts` 不能作为同步文档的 `created_by` 归属，也不能被授予 workspace write。原架构评估提的「服务账号归属」在此处不成立。

### 4.2 二选一决策

| 选项 | 做法 | 改动量 | 推荐 |
|---|---|---|---|
| **(A) 专用同步用户** | 部署期在 `users` 创建专用同步用户（如 `sync@feishu-connector`），通过 RBAC `Grant` 授其目标 workspace/directory 的 write/admin；连接器用该用户 `UserID` 构造 `AuthContext` 调 `DocumentService.Create/Update` | **零架构改动**（复用现有 RBAC + DocumentService） | **推荐** |
| (B) 扩展 RBAC 加 `SubjectServiceAccount` | `domain/rbac.go` 加 `SubjectServiceAccount`，`engine.Check` + `GrantsFor` SQL + `AuthContext` + `permissions.created_by` 全链路改 | 高（RBAC 域模型 + 引擎 + repo + DocumentService 全改） | 不推荐（P1 不值当） |

**推荐 (A)**：专用同步用户。理由：① 零架构改动，复用现有 RBAC 显式优先继承 + DocumentService；② `created_by` 落真实 `users.id`，RBAC 链路一致；③ 同步文档权限从落地 directory 继承（`inherit_scope=subtree`，`005_rbac`），不传递飞书/语雀的 sharing 模型——后者与 Mora RBAC 语义不一致，传递会造成存在性泄露；④ 部署期 admin 显式配置 `sync_connectors.owner_user_id` + 目标 directory + `default_role`，无主文档不进目录树。

### 4.3 权限映射策略（避免无主文档绕过存在性不泄露）

1. 每个连接器绑定一个**专用同步用户**（`owner_user_id`，真实 `users.id`），对其目标 directory 授 write（admin 显式配置）；
2. 同步文档 `created_by = owner_user_id`，权限**从落地 directory 继承**（Mora RBAC 显式优先），**不传递源端 sharing**——飞书/语雀的可见性模型不映射进 Mora；
3. `default_role`（read/write）决定落地后该 directory 下文档对其他用户的默认可见性，由部署期 admin 定，无主文档不进目录树、不在检索中可见（存在性不泄露保持）；
4. 同步写入全程走 `DocumentService.Create/Update` 的 RBAC 校验，不绕过；
5. 同步行为入 `audit_logs`（`actor_type='user'`/`actor_id=owner_user_id`，`action='sync_import'`）。

---

## 5. 密钥与不出网

- `sync_connectors.credentials`（app_secret/token）**不得明文落库**。复用现有配置体系（`internal/platform/config`）的密钥管理，或新增 `sync_connectors.credentials_enc` 字段（AES-GCM，密钥来自部署环境变量，参考 `07 §3` 存储加密策略）。**决策书定稿时需研发确认现有密钥封装复用点**（门控，见 §7）。
- 不出网：默认 `sync_connectors.base_url` 为空 → 同步禁用；用户显式配置私有部署实例地址 + 凭据后才出网，且仅限该地址（出网白名单 + 审计，`07 §6`）。

---

## 6. 门控与被低估改动汇总

| 项 | 状态 | 说明 |
|---|---|---|
| 依赖 #1（Parser + BlockTable） | ✅ 已就绪 | PR #5 已合，全量导入路径复用 |
| **`service_accounts` 非 RBAC subject** | ⚠️ 被低估改动（本文已解） | 改走专用同步用户 (A)，零架构改动；不复用 service_accounts 做归属 |
| 密钥封装复用点 | 🔶 待确认 | `credentials` 加密存取需研发确认现有密钥基建复用点 |
| 新增 3 张同步表 | 新增 | `sync_connectors`/`document_sources`/`sync_runs`，独立模块 `internal/module/sync` |
| `DocumentService.Create/Update` 主体改动 | 零 | 连接器构造 AuthContext 直接调，不改 service |
| RBAC 主体改动 | 零（选 A） | 专用同步用户走现有 Grant |
| 出网白名单 + 审计 | 复用 | `07 §6` 已设计，同步任务纳入 |

**结论**：#2 落入 P1 既有架构边界，路径最短、无未解外部门控（区别于 #3 的 draft/review 门控）。唯一被低估的改动是 §4.1 的 service_accounts 归属问题，已用专用同步用户 (A) 化解为零架构改动。

---

## 7. 风险与缓解

| 风险 | 等级 | 缓解措施 |
|---|---|---|
| 飞书/语雀 API 限流与分页 | 中 | 连接器实现指数退避 + 游标分页；`sync_runs` 记录进度，支持断点续传 |
| 源端结构映射不全（飞书新版式/语雀复杂块） | 中 | 复用 #1 Parser 的降级策略，未识别块降级为 paragraph，不阻塞导入 |
| 大空间全量导入超时 | 中 | 异步任务（复用 Valkey Streams 或专用 sync worker），`sync_runs` 跟踪 |
| 密钥泄露 | 高 | `credentials` 加密存（§5），明文不落库；专用同步用户最小权限 |
| 不出网原则违反 | 中 | 默认禁用，显式配置 + 出网白名单 + 审计 |
| 增量删除语义 | 中 | 源端删除不硬删 Mora 侧，标记/归档由人决定（不可改写版本模型） |

### 7.1 估期（架构层粗估，供 PM 排期，研发定稿为准）

| 项 | 估期 | 说明 |
|---|---|---|
| `sync_connectors`/`document_sources`/`sync_runs` 三表 + 迁移 | 1d | 独立 011 迁移 |
| 飞书连接器（API + 分页 + 限流） | 4–5d | 含私有部署实例适配、导出格式转 #1 Parser |
| 语雀连接器 | 3–4d | 复用飞书连接器骨架 |
| 增量对账逻辑（content_hash 比对 + 新版本） | 2d | 复用 `DocumentService.Update` |
| 审计集成 + 出网白名单配置 | 1d | 复用 `AuditRepo.Append` |
| 密钥封装复用确认 | 0.5d | 待研发确认现有基建 |
| 合计 | **~12d** | 飞书+语雀两个连接器，可与 #3 并行 |

---

> 本决策书为 #2 外部源同步草案。选型论证 + 同步映射模型 + 权限归属策略（专用同步用户化解 service_accounts 非 RBAC subject 的被低估改动）+ 增量对账机制已齐。密钥封装复用点为唯一待确认项（不阻塞选型）。研发可据此实现 `internal/module/sync` 模块与三表迁移。门控与低估改动已在 §6 汇总。
