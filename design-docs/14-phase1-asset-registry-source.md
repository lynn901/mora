# Phase 1 实施架构：统一资产与来源（Asset Registry / Source / 存量文档在线迁移）

> 对应 `design-docs/12-human-agent-knowledge-architecture.md` §16.2，蓝图 §12 Phase 1。
> 本文是 Phase 1 的**实施层架构决策书**，把 §19 中与本阶段相关的开放决策（#4 投影表、#5 在线迁移协议、#6 Connector 框架、#7 knowledge-worker、#8 Source API、#9 出网/SSRF）落到研发可直接实现的契约。
> 架构师负责本文；研发同学按本文实现，测试 / 交付部署按 §10 / §11 设计矩阵与门禁。
> 前置：Phase 0（`design-docs/13-phase0-contract-safety-baseline.md`）已落地 8 张核心表 + ResourceLocator + authz.Service + Outbox + knowledge_jobs 基础库。

## 0. 决策摘要

| # | 决策 | 结论 | 依据 / 权衡 |
|---|---|---|---|
| D9 | 投影/治理/来源/关系表归属 | 新增迁移 `014_phase1_asset_source.up/down.sql`，补齐 `knowledge_sources` / `source_sync_runs` / `knowledge_source_targets` / `knowledge_relations` / `review_requests` / `review_decisions` / `asset_projections` 7 表；`governance_profiles` 已在 013 落地，不重建，仅补 `legacy_migration` 系统行 | §16.2 第 1 项"在 Phase 0 核心表上补齐来源、SourceTarget、同步 Run、关系、治理和投影表" |
| D10 | 存量文档在线迁移协议 | 双写（新文档写事务同时登记 Document Asset）→ backfill（存量文档批量登记为 Document Asset，`dedupe_key=document_version:{version_id}`，`content_origin=human`，不复制正文）→ 对账（asset.current_version_id 与 document 当前版本一致性扫描）；存量迁移走 `legacy_migration` 治理 Profile（系统批准、可审计、不自动发布到团队） | §16.2 第 2 项 + §3.3"Document Asset 不复制 documents.content" |
| D11 | Connector 框架形态 | `internal/module/knowledge/source/connector` 端口（§7.1 `SourceConnector`）+ file / url_api / git 三 adapter；Connector 只负责"取得固定 revision 的输入"，不决定资产权限/发布/治理；`ContentSink` 由 Mora 提供，只允许写任务隔离目录或指定 MinIO 前缀 | §7.1 / §10.6"Connector 与类型 Provider 分离" |
| D12 | knowledge-worker 进程边界 | 新增 `cmd/knowledge-worker` 入口，复用同一 Go 仓库 + `Dockerfile`（TARGET=knowledge-worker）；独立消费组 `source_sync`（`source_events` Stream）+ 复用 `knowledge_projection`（`knowledge_events` Stream）；首版不出对外业务 API，只跑 Job；`outbox-dispatcher` 首版内嵌其 main | §2.2 进程职责表 + §2.3"outbox-dispatcher 首版可作为 mora-api 和 worker 内的后台组件运行" |
| D13 | Source 管理 REST API | 新增 `/api/v1/workspaces/{ws}/knowledge/sources` 等子集（§11.1），cursor 分页 + `Idempotency-Key` + ETag；创建/同步/审核操作支持幂等；同步默认产物只到 `candidate` | §11.1 + §16.2 第 4 项"默认只到 candidate" |
| D14 | 出网与 SSRF 防护 | 新增 `internal/platform/egress`：DNS 解析后阻止 loopback / link-local / metadata endpoint / 未批准私网；每次重定向重新校验；限制响应大小、类型、跳转次数；出网 allowlist + 超时 + 审计 + Secret 脱敏；Git 凭据一次性 helper 注入，不写入 remote URL/日志/仓库配置 | §7.3 Connector 安全 + §13.2 Secret 管理 |
| D15 | 版本原子切换不变量 | CAS 激活（`latest_requested_version_no` 单调栅栏，SQL 固定 §3.2）；required projections 全部 ready 才能 `build_status=ready`；同步构建失败/审核未完成/部分投影就绪都不得覆盖 `current_version_id`；查询始终解析 `current_version_id` | §6.4 资产版本原子切换 + §16.2 门禁"失败不替换最后可用版本" |
| D16 | 凭据隔离 | `credential_ref` 指向 Secret 管理器或加密凭据表，URI 持久化前移除内嵌凭据；Worker 获取短期解密值只存内存，Run 创建时固化 `credential_version`；Provider 不获 Source credential，LLM 不获 Agent Token | §13.2 + §7.2"Run 创建时必须固化已脱敏的 Source 配置、credential version" |

## 1. 与现有代码的差距核对（Phase 1 基线）

依据 `design-docs/12` §1.2，并经代码核对确认（`internal/infra/postgres/authz_repos.go`、`internal/platform/outbox/`、`migrations/013_knowledge_core.up.sql`）：

| 差距 | 代码现状（Phase 0 交付） | Phase 1 目标 |
|---|---|---|
| 来源表 | `knowledge_asset_versions.source_id` 列已建但**无 FK**（013 注释"Phase 1 才有 source 表，先建列不加 FK"） | 建 `knowledge_sources` 表 + 回补 `source_id` FK；建 `source_sync_runs` / `knowledge_source_targets` |
| 投影表 | 无投影登记表；RAG 投影（FTS/Qdrant）由 `documents.index_status` 单值表达 | 新增 `asset_projections`：每个版本 × 投影类型 × build_revision 一行，支持 ready/stale 状态机与对账 |
| 治理审核 | `governance_profiles` 已建，但无审核请求/决策记录表；版本 `governance_status` 无来源 | 新增 `review_requests` / `review_decisions`：每次批准/拒绝/合并/提升/废弃写不可变 decision；资产当前状态是决策的投影 |
| 关系 | 无资产间关系表 | 新增 `knowledge_relations`：`derived_from`/`explains`/`implements`/`supersedes`/`contradicts`/`uses`/`related_to`，不跨 workspace |
| 存量文档资产 | `documents`/`document_versions` 存在但未登记为 Asset；`DocWriteSink` 已双写 Knowledge Outbox 事件但无 Asset 写入 | 在线迁移：双写登记 + backfill 存量 + 对账；不复制正文，只建 `native_document_id`/`native_document_version_id` 引用 |
| Connector | 无来源摄取框架；`rag-worker` 只消费 `doc_events` 做解析/向量化 | 新增 `source/connector` 端口 + file/url_api/git adapter；`knowledge-worker` 调度 Connector + 路由到 Document/RAG pipeline |
| knowledge-worker | 无此进程；`cmd/` 只有 mora-api / rag-worker / mcp-server | 新增 `cmd/knowledge-worker`；复用 `worker.JobStore`（013 已落地）+ Outbox Dispatcher（013 已落地） |
| 出网防护 | 无统一 egress 层；rag-worker 直连 TEI/Qdrant/MinIO（同网受控） | 新增 `platform/egress`：URL/API/Git 来源必须经 egress 校验（DNS/重定向/私网/大小/类型/凭据脱敏/审计） |
| Source API | 无 Source 管理 REST 端点 | 新增 `/api/v1/workspaces/{ws}/knowledge/sources` 等子集 |

**回归红线**（§16.2 门禁"现有文档/RAG/MCP 行为回归无退化"）：
- `documents`/`document_versions` 表结构与 `DocWriteSink` 双写行为不变；存量迁移只**新增** Asset 行，不改 `documents` 列。
- `doc_events` Stream 与既有 RAG 发布器行为不变；`knowledge_events` Stream 的 Source 事件由 `knowledge-worker` 消费，不污染 `doc_events`。
- `rbac.Engine.Check`/`VisibleDocuments` 契约不变；Phase 0 的 `AssetLocator`/`AgentLocator` 行为不变，Phase 1 仅新增 `SourceLocator`。
- `authz.Service.Authorize` 决策流水线不变；Phase 1 只在其已支持的 `sync`/`review` 动作上接入 Source/Review 目标。

## 2. 数据架构：来源、投影、治理、关系表（D9）

迁移文件：`migrations/014_phase1_asset_source.up.sql` / `014_phase1_asset_source.down.sql`。
原则：只建表 + 约束 + 索引 + 回补 013 的 `source_id` FK；不写业务数据（仅补 `legacy_migration` 系统治理 Profile 行与默认 Source egress allowlist 种子，见 §2.2）。

### 2.1 表结构

```sql
-- 014_phase1_asset_source.up.sql
-- Phase 1 控制面表（12 §4.2 来源/投影/治理/关系，§7.1 Connector，§13.2 凭据）
-- 依赖：013_knowledge_core（knowledge_assets/versions、governance_profiles、knowledge_jobs、outbox_events）
--       003 documents/document_versions（native 引用）

-- 来源（12 §4.2 knowledge_sources）
CREATE TABLE knowledge_sources (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    source_type       VARCHAR(20) NOT NULL,   -- file | url_api | git
    name              VARCHAR(500) NOT NULL,
    uri_normalized    TEXT NOT NULL,          -- 已移除内嵌凭据
    credential_ref    TEXT,                  -- Secret 管理器/加密凭据表的引用，不含明文
    sync_policy       JSONB NOT NULL DEFAULT '{}',  -- schedule/cursor/rate/max_bytes
    trust_level       VARCHAR(20) NOT NULL DEFAULT 'untrusted', -- untrusted | trusted | internal
    license           JSONB,                 -- {spdx, status, notice_required}
    current_revision  TEXT,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    last_synced_at    TIMESTAMPTZ,
    last_error        TEXT,                  -- 已脱敏
    created_by_type   VARCHAR(20) NOT NULL,
    created_by_id     UUID NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_type IN ('file','url_api','git')),
    CHECK (trust_level IN ('untrusted','trusted','internal'))
);
CREATE UNIQUE INDEX uq_sources_workspace_uri ON knowledge_sources(workspace_id, source_type, uri_normalized);
CREATE INDEX idx_sources_workspace ON knowledge_sources(workspace_id) WHERE enabled = true;
CREATE INDEX idx_sources_sync_due ON knowledge_sources(workspace_id, last_synced_at)
    WHERE enabled = true;

-- 同步 Run（12 §4.2 source_sync_runs）
CREATE TABLE source_sync_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id           UUID NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
    requested_by_type   VARCHAR(20) NOT NULL,
    requested_by_id     UUID NOT NULL,
    requested_revision  TEXT,                -- 可空=latest
    resolved_revision   TEXT,                -- Connector 解析的实际 revision
    source_config_snapshot JSONB NOT NULL,   -- 已脱敏快照，Run 期间不可变
    credential_version  TEXT,                -- 凭据版本标识，凭据轮换不影响已排队 Run
    governance_profile_id UUID REFERENCES governance_profiles(id),
    requested_asset_type VARCHAR(20) NOT NULL, -- document | codebase | memory | skill
    status              VARCHAR(20) NOT NULL DEFAULT 'queued', -- queued|fetching|processing|ready|failed|cancelled
    attempt             INT NOT NULL DEFAULT 0,
    idempotency_key     TEXT NOT NULL UNIQUE,
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    error_code          VARCHAR(60),
    error_detail_redacted TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (status IN ('queued','fetching','processing','ready','failed','cancelled'))
);
CREATE INDEX idx_sync_runs_source ON source_sync_runs(source_id, created_at DESC);
CREATE INDEX idx_sync_runs_status ON source_sync_runs(status, started_at)
    WHERE status IN ('queued','fetching','processing');

-- 来源 Target → Asset 映射（12 §4.2 knowledge_source_targets）
-- Connector 每次同步必须返回稳定 target_key；资产不得仅靠标题或 URL 推断是否已有目标。
CREATE TABLE knowledge_source_targets (
    source_id      UUID NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
    target_key     TEXT NOT NULL,           -- Connector 返回的稳定键
    asset_type     VARCHAR(20) NOT NULL,
    asset_id       UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    selector       JSONB,                   -- 子路径/过滤等不可执行选择器
    active         BOOLEAN NOT NULL DEFAULT true,
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, target_key)
);
CREATE INDEX idx_source_targets_asset ON knowledge_source_targets(asset_id) WHERE active = true;

-- 资产关系（12 §4.2 knowledge_relations）
-- 关系不得跨 workspace；supersedes/contradicts 必须保留创建证据或人工决策。
CREATE TABLE knowledge_relations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    from_asset_id   UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    from_version_id UUID REFERENCES knowledge_asset_versions(id) ON DELETE SET NULL,
    relation_type   VARCHAR(30) NOT NULL,  -- derived_from|explains|implements|supersedes|contradicts|uses|related_to
    to_asset_id     UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    to_version_id   UUID REFERENCES knowledge_asset_versions(id) ON DELETE SET NULL,
    origin          VARCHAR(20) NOT NULL DEFAULT 'human', -- human|generated|system
    confidence      NUMERIC(5,4),
    created_by_type VARCHAR(20) NOT NULL,
    created_by_id   UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (relation_type IN ('derived_from','explains','implements','supersedes','contradicts','uses','related_to')),
    CHECK (origin IN ('human','generated','system')),
    CHECK (from_asset_id <> to_asset_id)
);
CREATE INDEX idx_relations_from ON knowledge_relations(from_asset_id) WHERE relation_type IN ('supersedes','contradicts');
CREATE INDEX idx_relations_to ON knowledge_relations(to_asset_id);
CREATE INDEX idx_relations_workspace ON knowledge_relations(workspace_id, relation_type);

-- 审核请求（12 §4.2 治理：每次批准/拒绝/合并/提升/废弃写不可变 decision）
CREATE TABLE review_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    asset_id        UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    asset_version_id UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    governance_profile_id UUID NOT NULL REFERENCES governance_profiles(id),
    requested_by_type VARCHAR(20) NOT NULL,
    requested_by_id UUID NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending|approved|rejected|superseded
    rationale       TEXT,                  -- 已脱敏的请求理由
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,
    resolved_by_type VARCHAR(20),
    resolved_by_id UUID,
    CHECK (status IN ('pending','approved','rejected','superseded'))
);
CREATE INDEX idx_review_requests_asset ON review_requests(asset_id, created_at DESC);
CREATE INDEX idx_review_requests_pending ON review_requests(workspace_id, status) WHERE status = 'pending';

-- 审核决策（不可变；资产当前状态只是这些决策的投影，12 §4.2）
CREATE TABLE review_decisions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_request_id UUID NOT NULL REFERENCES review_requests(id) ON DELETE CASCADE,
    decision          VARCHAR(20) NOT NULL,  -- approve|reject|merge|promote|deprecate
    decision_by_type  VARCHAR(20) NOT NULL,
    decision_by_id    UUID NOT NULL,
    policy_version    TEXT NOT NULL,         -- 治理 Profile 版本/快照引用
    rationale_redacted TEXT,                 -- 已脱敏
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (decision IN ('approve','reject','merge','promote','deprecate'))
);
CREATE INDEX idx_review_decisions_request ON review_decisions(review_request_id, created_at);

-- 资产投影（12 §4.5 asset_projections）
-- 任何投影都必须记录 asset_version_id/projection_kind/provider/provider_version/build_revision/built_at，
-- 以支持重建、对账和问题定位（§4.1）。
CREATE TABLE asset_projections (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_version_id UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    projection_kind VARCHAR(20) NOT NULL,    -- fts|vector|summary|codegraph|relation
    provider        VARCHAR(60) NOT NULL,
    provider_version TEXT,
    build_revision  TEXT NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending|building|ready|failed|stale
    locator         JSONB,                  -- 不可执行定位信息（collection/prefix/key 等）
    built_at        TIMESTAMPTZ,
    last_error      TEXT,                   -- 已脱敏
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (asset_version_id, projection_kind, build_revision),
    CHECK (projection_kind IN ('fts','vector','summary','codegraph','relation')),
    CHECK (status IN ('pending','building','ready','failed','stale'))
);
CREATE INDEX idx_projections_version ON asset_projections(asset_version_id, projection_kind);
CREATE INDEX idx_projections_stale ON asset_projections(status) WHERE status IN ('pending','building','stale');

-- 回补 013 的 source_id FK（013 建列时无 source 表，未加 FK）
ALTER TABLE knowledge_asset_versions
  ADD CONSTRAINT fk_versions_source
  FOREIGN KEY (source_id) REFERENCES knowledge_sources(id) ON DELETE SET NULL;
```

down 迁移按反序 `DROP TABLE`，先 `ALTER TABLE knowledge_asset_versions DROP CONSTRAINT fk_versions_source`，再 `DROP TABLE asset_projections / review_decisions / review_requests / knowledge_relations / knowledge_source_targets / source_sync_runs / knowledge_sources`。

### 2.2 关键不变量与约束说明

- **`knowledge_sources.uri_normalized` 已脱敏**：URI 持久化前必须移除内嵌凭据（`https://user:pass@host` → `https://host`），明文凭据只存 `credential_ref` 指向的 Secret 管理器或加密凭据表（§13.2）。`(workspace_id, source_type, uri_normalized)` 唯一，防止同源重复登记。
- **`source_sync_runs` 不可变快照**：Run 创建时必须固化已脱敏的 Source 配置、`credential_version`、`governance_profile_id`、`requested_asset_type`；后续编辑 Source 不影响已排队 Run（§7.2）。`idempotency_key` UNIQUE 保证重复同步请求幂等。
- **`knowledge_source_targets.(source_id, target_key)` 唯一**：Connector 每次同步必须返回稳定 `target_key`；资产不得仅靠标题或 URL 推断是否为已有目标（§4.2）。`asset_id` 引用 `knowledge_assets(id)`，同一 target 重新同步时 upsert 映射，不新建 Asset。
- **`knowledge_relations` 不跨 workspace**：`from_asset_id` 和 `to_asset_id` 必须同属一个 workspace（由应用层校验 + `workspace_id` 列固化）；`supersedes`/`contradicts` 必须保留 `created_by` 与 `review_decisions` 证据（§4.2）。
- **`review_decisions` 不可变**：每行只能追加，不更新不删除；`review_requests.status` 是最新决策的投影。资产 `knowledge_asset_versions.governance_status` 的 `published`/`rejected`/`deprecated` 转换必须有对应 `review_decisions` 行（应用层门禁）。
- **`asset_projections` 唯一性**：`(asset_version_id, projection_kind, build_revision)` 唯一，同一版本的同一投影类型同一构建 revision 只有一行；重建产生新 `build_revision`，不原地重写。`locator` 只保存不可执行定位信息（Qdrant collection/point filter、FTS 表名、MinIO key 前缀），不保存正文。
- **`legacy_migration` 治理 Profile**：迁移补一行系统治理 Profile，`is_system=true`、`asset_type=document`、`auto_publish={"legacy_migration": true}`，`review_roles=[]`，`required_projections=["fts","vector"]`；存量文档 backfill 走此 Profile，由系统服务账号批准（`approved_by_type=service_account`），`governance_status` 直接置 `published`（已是人工已发布文档），不进团队审核收件箱。
- **物理删除策略**：所有新表默认软删除（`enabled`/`active`/`status`/`resolved_at`）；`source_sync_runs`/`review_decisions` 保留 30 天后由对账任务清理；`knowledge_sources` 删除先 `enabled=false` 再按保留策略冻结资产（§12.2 删除矩阵）。

## 3. 存量文档在线迁移协议（D10）

### 3.1 双写（新文档）

`DocWriteSink.WriteDoc`（013 已落地）在文档写事务内已双写 Knowledge Outbox 事件。Phase 1 扩展同一事务：在 `outbox_events` 之前插入 `knowledge_assets` + `knowledge_asset_versions`，使文档与资产原子提交。

```text
DocWriteSink.WriteDoc(ctx, doc, version, prevVersion, create, ev)
  BEGIN tx
    INSERT/UPDATE documents + document_versions   -- 既有逻辑不变
    -- Phase 1 新增：登记 Document Asset（仅在 create=true 或新 version 时）
    IF create:
        INSERT knowledge_assets (workspace_id, asset_type='document',
              native_document_id=doc.id, status='published',
              governance_profile_id=<document_default_or_legacy>,
              owner_type='user', owner_id=doc.created_by,
              latest_requested_version_no=1)
        -- ev.Payload 已带 asset_id/version_id，供消费者激活
    ELSE IF new version:
        UPDATE knowledge_assets SET latest_requested_version_no = version_no
    INSERT knowledge_asset_versions (asset_id, version_no=doc.version_no,
          native_document_version_id=version.id, content_origin='human',
          dedupe_key='document_version:'||version.id,
          build_status='ready',        -- 原生文档版本可读即 ready
          governance_status='published', -- 原生文档默认已发布
          activation_policy_snapshot=<固化>,
          created_by_type='user', created_by_id=doc.created_by)
    outbox.Store.Record(tx, ev, ['knowledge_events'])  -- 既有
  COMMIT
```

**不变量**：`(asset_type='document', native_document_id)` 部分唯一索引（013 已建 `uq_assets_native_doc`）保证一个文档只登记一个 Asset；`(asset_id, native_document_version_id)` 唯一保证一个文档版本只登记一个资产版本。双写失败时整个事务回滚，文档与资产都不落地——不出现"有文档无资产"或"有资产无文档"的中间态。

### 3.2 Backfill（存量文档）

存量文档批量登记为 Document Asset，**不复制正文**，只建引用：

```text
对每个 workspace（按 id 分批，batch=500 文档/事务）:
  BEGIN tx
    -- 选该 workspace 下未登记 Asset 的文档（status != 'deleted'）
    SELECT d.id, d.workspace_id, d.title, d.created_by, d.version_no, v.id AS version_id
    FROM documents d JOIN document_versions v ON v.document_id=d.id AND v.version_no=d.version_no
    LEFT JOIN knowledge_assets a ON a.native_document_id=d.id AND a.asset_type='document'
    WHERE d.workspace_id=$1 AND a.id IS NULL
    LIMIT 500 FOR UPDATE SKIP LOCKED

    FOR EACH (d, v):
      INSERT knowledge_assets (..., native_document_id=d.id, status='published',
             governance_profile_id=<legacy_migration profile>, ...)
      INSERT knowledge_asset_versions (..., native_document_version_id=v.id,
             dedupe_key='document_version:'||v.id, build_status='ready',
             governance_status='published', created_by_type='service_account',
             created_by_id=<migration_service_account>)
      -- current_version_id CAS 激活（同一事务）
      UPDATE knowledge_assets SET current_version_id=<version_id>
        WHERE id=$asset_id AND latest_requested_version_no=$version_no
              AND current_version_id IS NULL   -- backfill 只设初始值
      INSERT review_requests + review_decisions  -- legacy_migration 系统批准记录
    INSERT outbox_events (asset.version.requested, ['knowledge_events'])  -- 触发投影构建
  COMMIT
```

`dedupe_key='document_version:'||version.id` 与双写一致，所以 backfill 与双写幂等共存：若 backfill 已跑过某文档，该文档后续的新版本由双写登记，`dedupe_key` 唯一约束阻止重复。

**不复制正文**：`knowledge_asset_versions` 只保存 `native_document_version_id` 引用，正文读取仍走 `documents.content`/`document_versions.content`（§3.3 文档兼容适配）。`generation_ref`/`provider_ref` 对原生文档留空。

### 3.3 对账（一致性扫描）

对账任务定期扫描，修复可安全重建的状态；涉及权限/审批/内容冲突时进人工处置，不自动改权威记录（§12.3）。

| 扫描项 | 修复动作 | 不修复（人工处置） |
|---|---|---|
| `documents` 存在但无对应 `knowledge_assets` | 登记为 Document Asset（走 backfill 路径） | — |
| `document_versions` 存在但无对应 `knowledge_asset_versions` | 登记版本（`dedupe_key` 幂等） | — |
| `knowledge_assets.current_version_id` 与 `documents.version_no` 不一致 | CAS 更新到文档当前版本 | 资产状态非 `published`（可能人工废弃） |
| `knowledge_asset_versions.build_status='ready'` 但 required projection 缺失 | 重投投影 Job | 投影持续 failed（Provider 故障） |
| `asset_projections.status='ready'` 但版本已 `superseded` | 标记 `stale`，异步清理 | — |
| `source_sync_runs.status='ready'` 但未产生 `knowledge_source_targets` | 标记 Run failed + 重投 | — |
| `knowledge_sources.enabled=true` 但 `last_synced_at` 远超 schedule | 触发同步 | 凭据已失效 |

对账由 `knowledge-worker` 的后台 ticker 调度（§5.2），与既有 `rag-worker` 的 `indexing_tasks` 补偿扫描互不重叠（前者管 Asset/Source 一致性，后者管 doc_events RAG 投影）。

### 3.4 legacy_migration 治理与可见性

存量迁移产生的 Asset `governance_status='published'`，但走 `legacy_migration` 系统治理 Profile：
- `review_requests.requested_by_type='service_account'`，`rationale='legacy_migration backfill'`。
- `review_decisions.decision='approve'`，`decision_by_type='service_account'`，`policy_version='legacy_migration-v1'`。
- 不进团队审核收件箱（`review_roles=[]`）；可审计（`review_decisions` 不可变）。
- 团队可见性由文档既有 RBAC 决定（`authz.Service` 对 `TargetAsset` 的 `read`/`use` 求交文档权限），迁移不改变可见范围。

## 4. Connector 框架与 Source 摄取（D11 / D13）

### 4.1 Connector 端口

`internal/module/knowledge/source/connector` 定义端口（§7.1）：

```go
// Package connector defines the Source Connector port (design-docs/12 §7.1).
// A Connector fetches a fixed-revision input into a Mora-provided ContentSink.
// It does NOT decide asset permissions, publish state, or governance profile —
// those are Mora's responsibility (不变量: Connector 与类型 Provider 分离, §10.6).
package connector

type SourceType string

const (
    SourceFile   SourceType = "file"
    SourceURLAPI SourceType = "url_api"
    SourceGit    SourceType = "git"
)

// SourceConnector is the port each source adapter implements.
type SourceConnector interface {
    Type() SourceType
    // Validate checks source config, network reachability, credentials and
    // license BEFORE any Run is queued (§7.2 鉴权 sync + 校验).
    Validate(ctx context.Context, req ValidateRequest) error
    // ResolveRevision resolves the source's current revision (or a requested
    // one) WITHOUT fetching content. The Run snapshot stores this.
    ResolveRevision(ctx context.Context, src Source) (Revision, error)
    // Fetch pulls the content at revision into sink. Each manifest entry MUST
    // carry a stable target_key, asset_type, content hash and content locator.
    Fetch(ctx context.Context, src Source, rev Revision, sink ContentSink) (FetchManifest, error)
    Health(ctx context.Context) error
}

// ContentSink is Mora-provided; connectors may ONLY write to the task-isolated
// dir or designated MinIO prefix (§7.1). It enforces size/quota limits.
type ContentSink interface {
    // Write returns a writer scoped to target_key; connector writes content
    // then closes. The sink computes content_hash on close.
    Write(ctx context.Context, targetKey string) (ContentWriter, error)
}

type FetchManifest struct {
    Revision Revision
    Entries  []ManifestEntry
}
type ManifestEntry struct {
    TargetKey   string        // 稳定键，幂等 upsert SourceTarget
    AssetType   string        // document | codebase | memory | skill
    ContentHash string
    Locator     Locator       // 不可执行：MinIO key / 临时路径
    Metadata    map[string]any // 不含凭据
}
```

**约束**：
- Connector 不接收 `allowed_asset_ids` 或用户 Token；只接收 Run 快照（已脱敏 Source 配置 + `credential_version`）。
- `ContentSink.Write` 强制大小限制（`sync_policy.max_bytes`）与隔离前缀；Connector 不可越权写其他路径。
- `FetchManifest.Entries[].TargetKey` 必须稳定（文件用归一化路径 hash，URL/API 用稳定 ID，Git 用 `repo+path`），不得用标题或时间戳。

### 4.2 摄取流程

§7.2 落地，由 `knowledge-worker` 的 `source_sync` Job 驱动：

```text
创建/更新 Source（API）
  -> 鉴权 sync + egress.Validate（网络/凭据/许可证/SSRF，§5）
  -> 快照 Source 配置 + credential_version + governance_profile + requested_asset_type
  -> INSERT source_sync_run(status='queued') + outbox(source.sync_requested, ['source_events'])
  -> COMMIT（Run 持久化，Worker 异步消费）

knowledge-worker 消费 source_events（source_sync 消费组）
  -> Acquire knowledge_jobs（dedupe_key = sync_run_id）
  -> 读 Run 快照（不读当前 Source 配置，防漂移）
  -> Connector.ResolveRevision（egress 校验）
  -> Connector.Fetch(rev, sink)（拉到隔离 MinIO 前缀，egress 审计每次出网）
  -> 计算 content_hash + manifest
  -> FOR EACH manifest entry:
       upsert knowledge_source_targets(source_id, target_key) -> asset_id
       按非空 dedupe_key 幂等创建 asset version:
         document:  dedupe_key = source:{source_id}:{target_key}:{rev}:{content_hash}
         codebase:  dedupe_key = codebase:{source_id}:{commit}:{content_hash}  (Phase 3)
       创建 required projection jobs（knowledge_events Stream）
  -> UPDATE source_sync_run(status='ready', resolved_revision=rev)
  -> 候选审核或可信来源策略批准（§4.3）
  -> 原子激活 current_version（§6，CAS）
```

导入内容默认 `governance_status='candidate'`。只有治理 Profile `auto_publish` 明确允许的可信来源（`trust_level='trusted'` 或 `'internal'`）才能自动批准；批准记录保存 `policy_version` 和执行主体。`untrusted` 来源必须人工审核（`review_requests.status='pending'`）。

### 4.3 file / url_api / git adapter 契约

| adapter | `target_key` 生成 | revision 语义 | 安全约束（§5） |
|---|---|---|---|
| **file** | 归一化路径 hash（`sha256(workspace_prefix + clean_path)`） | 文件 `mtime_ns + size` 或 `content_hash`（若 MinIO 已存） | MIME + 扩展名校验；压缩炸弹（解压比阈值）；路径穿越（`filepath.Clean` + 拒绝 `..`）；文件大小上限；解析在低权限环境 |
| **url_api** | URL 归一化 hash（去掉 query 中 cursor/token）或 API 返回的稳定 ID | HTTP `ETag`/`Last-Modified` 或 API cursor | DNS 解析后阻止 loopback/link-local/metadata/未批准私网；每次重定向重新校验；限制响应大小/类型/跳转次数（≤5）；分页/并发/速率/最大单次同步量；游标/revision 持久化 |
| **git** | `repo + branch + path`（content-addressed by commit） | `commit_sha` | 只允许批准协议（`https`/`git` over SSH，禁止 `file://`）与批准主机；凭据一次性 helper 注入，不写入 remote URL/日志/仓库配置；子模块策略校验；浅克隆 + commit 校验；不保留 Git 凭据（§7.4 Phase 3 保留无凭据 bundle） |

Phase 1 范围：file / url_api / git 三 adapter 的 `Validate`/`ResolveRevision`/`Fetch`/`Health` 实现；Git adapter 的 CodeGraph 构建留给 Phase 3，Phase 1 只到"登记 codebase Asset + commit revision"，不构建图。

### 4.4 Source 管理 REST API（D13）

§11.1 子集，所有批量列表 cursor 分页，写操作支持 `Idempotency-Key`，并发更新用 ETag：

```yaml
paths:
  /api/v1/workspaces/{ws}/knowledge/sources:
    get:    # 列表（cursor 分页）
      summary: 列出工作区来源
      parameters: [cursor, page_size, source_type, enabled]
    post:   # 创建来源（Idempotency-Key）
      summary: 创建来源（触发首次 sync Run，默认 candidate）
      requestBody: {source_type, uri, sync_policy, trust_level, license, requested_asset_type}
  /api/v1/knowledge/sources/{id}:
    get:    # 详情（含 current_revision, last_synced_at, last_error_redacted）
    patch:  # 更新（ETag/If-Match；credential_ref 单独换端点）
    delete: # 软删除（enabled=false + 资产冻结）
  /api/v1/knowledge/sources/{id}/sync-runs:
    get:    # Run 历史（cursor 分页）
    post:   # 触发新 Run（Idempotency-Key；requested_revision 可空=latest）
  /api/v1/knowledge/sources/{id}/credentials:
    put:    # 更新凭据（不读不返回明文，只存 credential_ref + 版本）
  /api/v1/workspaces/{ws}/knowledge/assets:
    get:    # 资产列表（cursor 分页；按 asset_type/status 过滤）
  /api/v1/knowledge/assets/{id}:
    get:    # 资产详情（含 current_version_id, versions 概览）
  /api/v1/knowledge/assets/{id}/versions:
    get:    # 版本历史
  /api/v1/knowledge/assets/{id}/relations:
    get:    # 关系（from/to，按 relation_type 过滤）
  /api/v1/workspaces/{ws}/knowledge/reviews:
    get:    # 待审核列表（status=pending）
  /api/v1/knowledge/reviews/{id}/decisions:
    post:   # 审核决策（approve/reject/merge/promote/deprecate；Idempotency-Key）
```

**错误语义**（§11.4 子集）：只读资源无权或不存在 → 404 + `code=40400`，不泄露存在性；写/治理动作无权限 → 403 + `code=40300`，写审计；版本构建中 → 200 + `stale/building` 标记，返回最后可用版本。

## 5. knowledge-worker 进程（D12）

### 5.1 进程职责与边界

| 职责 | 不负责 |
|---|---|
| 消费 `source_events` Stream（消费组 `source_sync`）调度 Connector | 对外业务 API（Source 管理 REST 在 mora-api） |
| 消费 `knowledge_events` Stream（消费组 `knowledge_projection`）编排资产投影 | 最终授权决策（`authz.Service` 在 mora-api） |
| Source 摄取：调 Connector、算 manifest、upsert SourceTarget/Asset、创建版本 | 决定资产权限/发布/治理（由治理 Profile + 审核） |
| 投影编排：创建/校验 `asset_projections`、CAS 激活 `current_version_id` | 具体投影构建（Document FTS/Qdrant 由 `rag-worker`，CodeGraph 由 Provider，Phase 3） |
| 对账扫描（§3.3） + Outbox Dispatcher（首版内嵌） | 替换 `rag-worker` 的 `doc_events` 消费 |
| legacy_migration backfill（一次性 + 增量对账） | — |

`knowledge-worker` 与 `rag-worker` 使用同一 Go 仓库（`go.mod`）和基础镜像（`Dockerfile`，新增 `TARGET=knowledge-worker` 构建参数），但保持独立命令、独立消费组、独立水平扩缩容策略（§2.2）。

### 5.2 Job dispatch

复用 Phase 0 的 `worker.JobStore`（`internal/infra/postgres/knowledge_job.go`，013 已落地 `Create`/`Acquire`/`Renew`/`Release`/`MarkSucceeded`/`MarkFailed`）。Phase 1 新增 job_type 处理逻辑：

| job_type | dedupe_key 形态 | 处理 | 投影产出 |
|---|---|---|---|
| `source_sync` | `sync:{source_id}:{requested_revision_or_latest}` | 调 Connector 摄取 | `asset_projections(fts,vector)` pending jobs |
| `projection_build` | `proj:{asset_version_id}:{projection_kind}:{build_revision}` | 调 rag-worker Document pipeline 或 Provider | `asset_projections.status` ready/failed |
| `asset_activate` | `activate:{asset_version_id}` | CAS 激活 `current_version_id`（§6） | — |
| `reconcile_scan` | `reconcile:{workspace_id}:{scan_kind}` | 对账扫描（§3.3） | 修复或人工处置 |
| `legacy_backfill` | `backfill:{workspace_id}:{doc_batch}` | 存量文档登记（§3.2） | `asset.version.requested` events |

幂等键格式 `job_type + asset_version_id + target_key + build_revision`（§6.5）。长任务用 `lease_owner`/`lease_until`（`DefaultLeaseTTL=5m`，可配置）；Worker 崩溃后 `Acquire` 的 `lease_until < now` 分支重新领取。

### 5.3 部署（Compose / K8s）

`deployments/docker-compose.yml` 新增 `knowledge-worker` 服务：

```yaml
  knowledge-worker:
    build:
      context: ..
      dockerfile: deployments/Dockerfile
      args:
        TARGET: knowledge-worker
    depends_on:
      postgres: { condition: service_healthy }
      valkey: { condition: service_healthy }
      minio: { condition: service_healthy }
      tei: { condition: service_started }
      migrate: { condition: service_completed_successfully }
    environment:
      DATABASE_URL: "postgres://mora:${POSTGRES_PASSWORD:-mora}@postgres:5432/mora?sslmode=disable"
      VALKEY_URL: "valkey:6379"
      QDRANT_URL: "http://qdrant:6333"
      TEI_URL: "http://tei:8080"
      EMBEDDING_PROVIDER: "${EMBEDDING_PROVIDER:-tei}"
      EMBEDDING_MODEL: "${EMBEDDING_MODEL:-sentence-transformers/all-MiniLM-L6-v2}"
      EMBEDDING_DIM: "${EMBEDDING_DIM:-384}"
      CONSUMER_NAME: "knowledge-worker-1"
      HEALTH_ADDR: ":8083"
      MINIO_ENDPOINT: "http://minio:9000"
      MINIO_ACCESS_KEY: "${MINIO_ACCESS_KEY:-mora}"
      MINIO_SECRET_KEY: "${MINIO_SECRET_KEY:-mora-secret}"
      MINIO_BUCKET: "mora"
      MINIO_SECURE: "false"
      # 出网白名单（egress allowlist，§6）
      EGRESS_ALLOW_DOMAINS: "${EGRESS_ALLOW_DOMAINS:-}"
      EGRESS_ALLOW_PRIVATE_RANGES: "${EGRESS_ALLOW_PRIVATE_RANGES:-false}"
      EGRESS_MAX_REDIRECTS: "5"
      EGRESS_MAX_RESPONSE_BYTES: "${EGRESS_MAX_RESPONSE_BYTES:-104857600}"  # 100MB
      EGRESS_TIMEOUT: "${EGRESS_TIMEOUT:-30s}"
    healthcheck:
      test: ["CMD", "/app/healthcheck", "http://localhost:8083/healthz"]
      interval: 15s
      timeout: 3s
      retries: 20
      start_period: 10s
    restart: unless-stopped
```

K8s Helm（`deployments/chart/mora/values.yaml`）新增对应 Deployment，与 `rag-worker` 同模板不同 `TARGET` 与消费组，独立 HPA。

`Dockerfile` 复用既有多阶段构建，新增 `TARGET=knowledge-worker` 分支编译 `cmd/knowledge-worker/main.go`；`outbox-dispatcher` 首版内嵌 `knowledge-worker.main` 的后台 goroutine（§2.3），规模增长后再独立 `cmd/outbox-dispatcher`。

## 6. 出网与 SSRF 防护（D14）

### 6.1 egress 端口

新增 `internal/platform/egress`：

```go
// Package egress is the network egress policy layer (design-docs/12 §7.3,
// §13.1 信任边界 5). All outbound calls to URL/API/Git sources MUST route
// through an EgressClient — it enforces DNS/redirect/private-network/size/
// type/timeout/allowlist/audit/secret-redaction checks. Provider adapters
// (TEI/Qdrant/MinIO) on the trusted internal network do NOT route through
// egress (they are same-network, separately credentialed).
package egress

type Policy struct {
    AllowDomains       []string        // 允许主机（精确或 *.suffix）
    AllowPrivateRanges bool            // 是否允许私网（默认 false；internal trust_level 可开）
    MaxRedirects       int             // 默认 5；每次重定向重新校验
    MaxResponseBytes   int64           // 默认 100MB
    AllowedContentTypes []string       // 来源期望的响应类型
    Timeout            time.Duration   // 默认 30s
}

type Client struct { /* net.Resolver + http.Client + audit sink */ }

// FetchURL fetches a URL under policy. Each redirect is re-validated: DNS is
// resolved and checked against loopback/link-local/metadata/未批准私网 BEFORE the
// connection is opened. Response size and content-type are enforced. Every
// egress is audited (redacted URL, status, bytes, duration).
func (c *Client) FetchURL(ctx context.Context, rawURL string, pol Policy) (*Response, error)

// DialHook returns a net.Dialer.Control hook that blocks private/metadata
// destinations at the socket layer — defense in depth for non-HTTP protocols
// (git over SSH, custom API).
func (c *Client) DialHook(pol Policy) func(network, addr string) (net.Conn, error)
```

### 6.2 URL/API 安全

- DNS 解析后阻止：`127.0.0.0/8`、`10.0.0.0/8`（除非 `AllowPrivateRanges`）、`172.16.0.0/12`、`192.168.0.0/16`、`169.254.0.0/16`（link-local）、`::1`、`fc00::/7`、`fe80::/10`、云 metadata endpoint（`169.254.169.254`）。
- 每次重定向（≤ `MaxRedirects`）重新解析 DNS 并校验，防止 TOCTOU（首次解析公网 → 重定向到内网）。
- 限制响应大小（`MaxResponseBytes`，流式读超额即断）、`Content-Type`（在 `AllowedContentTypes` 内）、跳转次数。
- 分页/并发/速率/最大单次同步量由 `sync_policy` 强制（`max_bytes`、`rate_per_sec`、`max_items_per_sync`）。
- 游标/revision 持久化到 `source_sync_runs.resolved_revision` 或 `knowledge_sources.current_revision`。

### 6.3 Git 安全

- 只允许批准协议：`https://`（egress 校验主机）、`git@` SSH（`DialHook` 校验）。禁止 `file://`（本地文件协议来源走 file adapter，不走 git adapter）。
- 凭据一次性 helper 注入：`GIT_ASKPASS` 或 `git credential` helper 在 fetch 期间临时生效，**不写入** remote URL、`.git/config`、日志或仓库配置；fetch 完成后立即清理。
- 子模块策略：默认禁止子模块（`--ignore-submodules`），需开启时单独校验每个子模块 URL 经 egress。
- 浅克隆（`--depth=1` + `--branch`）+ commit SHA 校验（确认 fetch 到的 commit 与 `ResolveRevision` 一致）。

### 6.4 文件安全

- MIME + 扩展名双校验（不信单一信号）。
- 压缩炸弹：解压比阈值（如 100:1）+ 解压后总大小上限。
- 路径穿越：`filepath.Clean` + 拒绝 `..` + 限制根目录。
- 文件大小上限（`PARSE_MAX_FILE_MB` 既有配置复用）。
- 解析在低权限环境（既有 `rag-worker` parser 模式，无网络无写主目录）。

### 6.5 凭据隔离（D16）

- `knowledge_sources.credential_ref` 指向 Secret 管理器（K8s Secret / External Secrets）或加密凭据表（PostgreSQL 加密列 + 信封加密），**不存明文**。
- URI 持久化前移除内嵌凭据（§2.2）。
- Worker 获取短期解密值只存内存，进程退出即消失；`source_sync_runs.credential_version` 记录所用凭据版本，凭据轮换不影响已排队 Run。
- Provider（CodeGraph/Extraction，Phase 3+）不获 Source credential；LLM 不获 Agent Token；Connector 也不获治理或授权凭据。
- 审计、错误、trace attributes 统一脱敏（URL 去 userinfo、凭据字段打码、`last_error` 字段已脱敏）。

## 7. 版本原子切换与投影就绪（D15）

§6.4 落地。不变量：**构建失败、审核未完成或部分投影就绪都不得覆盖最后可用版本**。

```text
创建 version(build_status='pending', governance_status='candidate')
  -> 快照 activation_policy_snapshot（治理 Profile 版本 + required_projections + auto_publish）
  -> 创建所需 projection jobs（knowledge_events Stream）
  -> 各 Provider/rag-worker 写版本化投影（asset_projections.status='building')
  -> projection status='ready'（每个投影类型一行 ready）
  -> 校验 required_projections 全部 ready -> UPDATE version.build_status='ready'
  -> 人工审核或可信来源策略批准 -> governance_status='published'
  -> 两个条件均满足时 CAS 激活 current_version_id：
     UPDATE knowledge_assets
     SET current_version_id = $1
     WHERE id = $2
       AND latest_requested_version_no = $3
       AND current_version_id IS NOT DISTINCT FROM $4   -- expected_current
  -> 异步清理被替代版本的可删除投影（status='stale'）
```

- `latest_requested_version_no` 单调栅栏：旧版本晚完成时 CAS 失败（`expected_current` 不匹配），只标记 `ready` 而不切换，防止旧构建覆盖新版本。
- 人工回滚是独立治理动作，必须显式指定目标版本和 `expected_current`（走 `review_decisions.decision='promote'` 或 `deprecate`）。
- 查询始终解析 `current_version_id`；`build_status` 非 `ready` 或 `governance_status` 非 `published` 的版本不作为查询结果（§11.4"版本构建中 → 返回最后可用版本和 `stale/building` 标记"）。
- required projections 与阻塞/非阻塞映射（§4.2 首版默认激活要求）：Document 阻塞投影 = 原生/导入内容版本可读且治理已发布；非阻塞 = FTS/vector/summary（未就绪时该能力降级，不覆盖旧版本）。

## 8. 模块与目录组织（Phase 1 子集）

§3.1 落地 Phase 1 必需部分：

```text
cmd/
  knowledge-worker/          # 新增：main.go（Job 消费 + Outbox Dispatcher + 对账 ticker）
  mora-api/                  # 既有；wiring.go 注册 Source/Asset/Review handler
  rag-worker/                # 既有；新增 asset_projections 桥接（Document 投影 ready 回写）
  mcp-server/                # 既有；Phase 1 不新增工具

internal/domain/
  knowledge_source.go        # 新增：Source/SyncRun/SourceTarget/Relation 值对象
  governance.go              # 新增：ReviewRequest/ReviewDecision 值对象
  projection.go              # 新增：AssetProjection 值对象

internal/module/knowledge/
  asset/                     # 新增：Registry、版本切换 CAS、生命周期
  source/                    # 新增：来源配置与同步编排
    connector/               # 新增：file / url_api / git adapter
  relation/                  # 新增：显式关系与冲突关系
  governance/               # 新增：profile（已建表）、审核、发布、废弃
  handler/                   # 新增：Source/Asset/Review REST handlers
  worker/                    # 既有（013 store.go）；扩展 job_type dispatch

internal/platform/
  egress/                    # 新增：出网与 SSRF 防护（§6）

internal/infra/
  postgres/
    knowledge_source.go      # 新增：7 表 repository（014 迁移）
    asset_registry.go        # 新增：Asset/Version 写入 + CAS 激活
  connector/                 # 新增：file/git/url_api adapter 实现（§4.3）

migrations/
  014_phase1_asset_source.up.sql / .down.sql
```

依赖规则（§3.2）：领域模块只依赖 `domain`、端口接口和基础平台；`source/connector` 不导入 `infra/connector`；`mcp` 不直接调 Source/Asset repository，走 mora-api 内部 client。

## 9. 与 Phase 0 authz.Service 的衔接

Phase 1 新增 `SourceLocator`（`TargetSource`）与 `ReviewLocator`（`TargetReview`），注册到既有 `CompositeLocator`（`cmd/mora-api/wiring.go`）：

- `SourceLocator.Locate(source_id)` → `[source, workspace]`，读 `knowledge_sources.workspace_id`；不存在返回 `ErrTargetNotFound`（存在性不泄露）。
- `ReviewLocator.Locate(review_id)` → `[review, workspace]`，读 `review_requests.workspace_id`。
- `authz.Service.Authorize` 的 `sync` 动作对 `TargetSource` 求交：用户/服务账号需有 workspace 级 `sync` 权限；`review` 动作对 `TargetReview` 求交治理 Profile 的 `review_roles`。
- 不新增 `TargetEvidence`（Phase 4），`EvidenceLocator` 保持 013 的 `ErrTargetNotFound` 占位。

## 10. 安全测试矩阵（交付测试工程师）

§16.2 第 5 项 + §17.4 安全测试。以下用例必须自动化通过：

### 10.1 SSRF / 出网

| # | 用例 | 预期 |
|---|---|---|
| 1 | URL 来源指向 `http://127.0.0.1` | `Validate` 拒绝；审计记录 |
| 2 | URL 来源指向 `http://169.254.169.254`（metadata） | `Fetch` 拒绝；socket 层 `DialHook` 阻断 |
| 3 | URL 首次解析公网，302 重定向到 `http://10.0.0.1` | 重定向重新校验，拒绝；不发起内网连接 |
| 4 | DNS rebinding（首次 A 记录公网、第二次内网） | `DialHook` 在 connect 时重新解析，阻断内网 |
| 5 | URL 来源响应超过 `MaxResponseBytes` | 流式读超额即断；`source_sync_runs.error_code='response_too_large'` |
| 6 | URL 来源 `Content-Type` 不在 allowlist | 拒绝；Run failed |
| 7 | URL 来源重定向次数 > `MaxRedirects` | 拒绝 |
| 8 | `EGRESS_ALLOW_DOMAINS` 未包含目标主机 | 拒绝；审计 |
| 9 | `EGRESS_ALLOW_PRIVATE_RANGES=false` 时指向 `192.168.x` | 拒绝；`trust_level='internal'` + 显式开启时放行 |
| 10 | Git 来源 `file://` 协议 | 拒绝（走 file adapter） |
| 11 | Git 凭据出现在 `.git/config` / 日志 | 不出现；fetch 后清理 |
| 12 | 文件来源压缩炸弹（100:1） | 拒绝 |
| 13 | 文件来源路径穿越（`../etc/passwd`） | 拒绝 |
| 14 | 文件来源 MIME/扩展名不匹配 | 拒绝 |

### 10.2 凭据隔离

| # | 用例 | 预期 |
|---|---|---|
| 15 | `knowledge_sources.uri_normalized` 含内嵌凭据 | 持久化前移除；明文不落库 |
| 16 | `source_sync_runs` 读到的是 Run 快照，Source 配置被改后不影响已排队 Run | Run 用快照配置 |
| 17 | 凭据轮换后旧 Run 仍用 `credential_version` 对应的值 | 不影响；新 Run 用新版本 |
| 18 | `last_error` / 日志 / trace 含凭据明文 | 脱敏（打码） |
| 19 | Connector 收到 `allowed_asset_ids` 或用户 Token | 拒绝（接口不接受） |

### 10.3 版本原子切换

| # | 用例 | 预期 |
|---|---|---|
| 20 | required projection 缺失时 `build_status` 转 `ready` | 阻止；门禁拒绝 |
| 21 | 构建失败时 `current_version_id` 被覆盖 | 不覆盖；指向最后可用版本 |
| 22 | 旧版本晚完成（`latest_requested_version_no` 已前进）CAS | 失败；只标记 `ready` 不切换 |
| 23 | `governance_status='candidate'` 版本被查询返回 | 不返回；返回最后 `published` 版本 |
| 24 | 人工回滚未显式 `expected_current` | 拒绝 |

### 10.4 越权与存在性不泄露（Phase 1 扩展）

| # | 用例 | 预期 |
|---|---|---|
| 25 | 无 `sync` 权限调 Source 创建/同步 | 403 + 审计 |
| 26 | 无 `read` 权限调 `GET /knowledge/assets/{id}` | 404（不泄露存在） |
| 27 | 跨 workspace 引用 asset/source/relation | 404（不泄露） |
| 28 | 撤权 Source（`enabled=false`）后下一次同步请求 | 同步拒绝 |
| 29 | `review` 动作不在 `review_roles` 中的主体 | 403 |

### 10.5 回归（无退化，§8.3 扩展）

- 现有 `rbac/engine_test.go` 全绿。
- `doc_events` RAG 索引链路不变；`rag-worker` 行为不变。
- MCP `search_knowledge_base`/`get_document`/`list_documents` 行为不变。
- `DocWriteSink.WriteDoc` 双写行为不变；存量文档双写新增 Asset 行不影响文档读/写/协同。

## 11. 交付清单与角色分工

| 交付项 | 主导 | 协同 | 验收 |
|---|---|---|---|
| 1. 014 迁移 7 表 + 回补 source_id FK | 架构师（本文 §2） | 后端研发实现 | 迁移 apply 成功；约束/索引齐；`legacy_migration` Profile 行就位 |
| 2. 在线迁移协议（双写 + backfill + 对账） | 架构师（本文 §3） | 后端研发实现 | 存量文档 100% 登记 Asset；`current_version_id` 与 `documents.version_no` 一致；不复制正文 |
| 3. Connector 框架 + file/url_api/git adapter | 架构师（本文 §4） | 后端研发实现 | 契约测试：revision/幂等/大小限制/错误分类 |
| 4. Source 管理 REST API | 架构师（本文 §4.4） | 后端研发实现 | cursor 分页 + Idempotency-Key + ETag；默认 candidate |
| 5. knowledge-worker 进程 + Job dispatch | 架构师（本文 §5） | 后端研发实现；交付部署（Compose/K8s） | Job 幂等/租约/重试分类；HPA 可扩缩 |
| 6. 出网与 SSRF 防护（egress） | 架构师（本文 §6） | 后端研发实现 | §10.1 SSRF 用例 100% 拒绝 |
| 7. 版本原子切换 + 投影就绪 | 架构师（本文 §7） | 后端研发实现 | §10.3 切换用例全通过 |
| 8. 安全测试矩阵 | **测试工程师** | 架构师提供本文 §10 | §10 全部用例自动化通过 |
| 9. knowledge-worker 部署 | **交付部署工程师** | 架构师提供本文 §5.3 | Compose `docker compose up` 拉起；K8s Helm 部署；healthcheck 通过 |
| 10. 统一资产列表/详情基础 UI | **前端开发** | 架构师提供 §4.4 API 契约 | 资产列表/详情/版本历史/审核收件箱可渲染 |

## 12. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 双写在文档写事务增加延迟 | Asset INSERT 轻量（2 行 JSONB）；CAS 同事务；监控 `DocWriteSink` 写延迟；必要时异步化 Asset 登记（牺牲原子性，降级为对账兜底） |
| backfill 大事务锁表 | 按 workspace 分批（batch=500）+ `FOR UPDATE SKIP LOCKED`；低峰执行；可暂停/续跑（`dedupe_key` 幂等） |
| Git 凭据泄露面 | 一次性 helper + `DialHook` + 不写 remote URL/config/日志；fetch 后清理；审计扫描 `.git/config` |
| SSRF 绕过（DNS rebinding、IPv6 等价） | `DialHook` 在 connect 时重新解析；IPv6 私网段校验；metadata endpoint 显式阻断；每次重定向重新校验 |
| `asset_projections` 与 Qdrant/FTS 不一致 | 对账扫描（§3.3）+ `rag-worker` 投影 ready 回写；版本 `superseded` 后投影标记 `stale` 异步清理 |
| `knowledge-worker` 与 `rag-worker` 投影职责重叠 | 边界明确：`knowledge-worker` 编排（创建/校验/激活），`rag-worker` 执行 Document FTS/vector 构建（复用既有 pipeline）；两者通过 `knowledge_jobs` 与 `asset_projections` 表协作 |
| 第三方 Connector 引入传染性 License | 第三方治理门禁（Phase 0 §6 已建）覆盖 Connector 依赖；lock.json + SBOM + NOTICE；AGPL/GPL 不引入 |
| Outbox Dispatcher 内嵌 `knowledge-worker` 单点 | 首版内嵌可接受（单机试用 ≤50 人）；规模增长后独立进程 + 多副本（`FOR UPDATE SKIP LOCKED` 已支持） |

## 13. 验收门禁（§16.2）

- [ ] 每个资产版本可追溯到来源 revision（`knowledge_asset_versions.source_revision` 非空 for 同步来源；原生文档 `native_document_version_id` 非空）。
- [ ] 同步构建失败时，`current_version_id` 与查询结果仍指向最后一个可用版本（不覆盖）。
- [ ] URL/API/Git Connector 的 SSRF、重定向、私网、出网白名单、凭据隔离与审计用例全部通过（§10.1 + §10.2）。
- [ ] 存量文档 100% 登记为 Document Asset；不复制正文；`dedupe_key` 幂等无重复。
- [ ] 现有文档/RAG/MCP 行为回归无退化（§10.5）。
- [ ] knowledge-worker Compose/K8s 部署拉起，healthcheck 通过。

## 14. 附录 A：与 §19 开放决策的对应

| §19 决策 | 本文落点 | 结论 |
|---|---|---|
| #4 投影表 | §2.1 `asset_projections` | `(asset_version_id, projection_kind, build_revision)` 唯一；`locator` 不可执行；`status` 状态机支持 ready/stale |
| #5 在线迁移协议 | §3 | 双写（`DocWriteSink` 同事务登记）+ backfill（分批 + `dedupe_key` 幂等）+ 对账（`current_version_id` 一致性扫描）；`legacy_migration` 系统治理 Profile |
| #6 Connector 框架 | §4.1 | `SourceConnector` 端口 + `ContentSink`（Mora 提供，隔离前缀）；file/url_api/git adapter；不决定权限/发布/治理 |
| #7 knowledge-worker | §5 | `cmd/knowledge-worker`；复用 `worker.JobStore` + Outbox Dispatcher（内嵌）；`source_sync` + `knowledge_projection` 消费组 |
| #8 Source 管理 API | §4.4 | `/api/v1/workspaces/{ws}/knowledge/sources` 子集；cursor + Idempotency-Key + ETag；默认 candidate |
| #9 出网/SSRF | §6 | `internal/platform/egress`；DNS + 重定向 + 私网 + 大小/类型 + allowlist + 审计 + 凭据脱敏；`DialHook` socket 层防御 |

其余 §19 决策（#10 Memory、#11 Skill、#12 task scope）属于 Phase 3+，本文不涉及。

---

## 附录 B：与现有 Phase 0 代码的对接点

| Phase 0 代码 | Phase 1 对接 |
|---|---|
| `migrations/013_knowledge_core.up.sql`（8 表） | 014 迁移回补 `knowledge_asset_versions.source_id` FK；`governance_profiles` 复用，补 `legacy_migration` 行 |
| `internal/platform/authz/service.go`（authz.Service） | 新增 `SourceLocator`/`ReviewLocator` 注册到 `CompositeLocator`；`sync`/`review` 动作接入新目标 |
| `internal/platform/authz/asset_agent_locator.go` | 新增 `source_locator.go`/`review_locator.go` 同模式 |
| `internal/platform/outbox/store.go` + `dispatcher.go` | `knowledge-worker` 内嵌 Dispatcher；Source 事件走 `source_events` Stream |
| `internal/module/knowledge/worker/store.go`（JobStore 端口） | `knowledge-worker` 扩展 job_type dispatch；复用 `Create`/`Acquire`/`MarkFailed` |
| `internal/infra/postgres/authz_repos.go`（AssetRepo 等） | 新增 `knowledge_source.go`（7 表 repository）+ `asset_registry.go`（写 + CAS） |
| `internal/infra/postgres/doc_write_sink.go`（DocWriteSink） | 扩展 `WriteDoc`/`DeleteDoc` 同事务登记 Asset/Version；`ev.Payload` 带 `asset_id`/`version_id` |
| `cmd/rag-worker/main.go`（rag-worker） | 新增 asset_projections 桥接：Document 投影 ready 回写 `asset_projections.status='ready'` |
| `cmd/mora-api/wiring.go`（CompositeLocator 注册） | 注册 Source/Review locator；注册 Source/Asset/Review handler |
| `deployments/docker-compose.yml` | 新增 `knowledge-worker` 服务；新增 egress 环境变量 |
| `deployments/Dockerfile` | 新增 `TARGET=knowledge-worker` 构建分支 |
