# Phase 2 架构设计：Wiki 增量维护（维护器提候选、locked 页保护、确定性 index/log）

> 文档版本：v1.0 ｜ 产出人：Mora 知识库架构师 ｜ 对应任务：YS-96
> 依据：design-docs/12-human-agent-knowledge-architecture.md §4.2（Wiki 维护表）、§7.5（Wiki 增量维护）、§10.4（Wiki Maintenance Provider）、§16.3（Phase 2 门禁）｜ 技术选型：PostgreSQL 16 + Valkey Streams + Qdrant + 复用 Phase 0/1 资产/版本/投影骨架

---

## 0. 决策摘要

| # | 决策 | 结论 | 依据 / 权衡 |
|---|---|---|---|
| D1 | 表归属 | 新增迁移 `016_phase2_wiki_maintenance.up/down.sql`（编号避让 Phase 1 已占用的 `015_asset_version_updated_at`），建 `wiki_spaces` / `wiki_pages` / `wiki_page_sources` / `wiki_maintenance_runs` / `wiki_page_proposals` 五表；Wiki 不新增 `asset_type`，页面复用 `knowledge_assets(asset_type='document')` | §0 决策 11「Wiki 是 Document 的维护方式」；14 已落地资产/版本骨架 |
| D2 | 维护器契约边界 | Provider 只接收 Mora 授权裁剪后的只读输入快照 + 不可执行 Schema，只返回受 JSON Schema 约束的 `PagePatch[]`；不触碰 DB/对象存储/URL/Git | §10.4；防 prompt injection 扩大读取范围 |
| D3 | locked 页保护 | `automation_state='locked'` 的页面，维护器只能产旁路建议（`page_kind='log'` 或独立 proposal），永不创建覆盖性候选 DocumentVersion | 不变量 11；门禁「locked 页自动覆盖为 0」 |
| D4 | 逐页 CAS 激活 | 每页独立 CAS `knowledge_assets.current_version_id`，沿用 Phase 1 `latest_requested_version_no` 单调栅栏；Run 非全局事务，失败页保留旧版本 | §12.1「每页候选与 CAS 独立原子」；门禁「部分失败不替换最后已发布页面」 |
| D5 | 幂等键 | Run 级 `input_set_hash + schema_version_id + model_revision + prompt_revision` 决定幂等；页级 `dedupe_key = wiki:{space}:{page_key}:{input_set_hash}:{maintainer_revision}`（复用 013 的 `knowledge_asset_versions.dedupe_key`） | §7.5；14 §2.1 dedupe_key 定义 |
| D6 | 确定性 index/log | index Document 由已发布页面确定性重建（确定性排序键 + 稳定哈希）；log Document 由 Run/Decision 事件追加；两者也是 `asset_type='document'` 的 Document Asset，不模型自由改写 | §4.2「index/log 也是可审计 Document」 |
| D7 | 复用投影 | Wiki 页面作为 `documents` 行 → FTS 免费复用 `idx_documents_fts`；Qdrant 复用 `mora_chunks_*` 与确定 point id（`uuid5`）；投影状态走 `asset_projections` | 05 §3.5/§4.1；14 §2.1 asset_projections |
| D8 | 事件流 | 新增 `wiki_events` Stream + `wiki_maintenance` 消费组（knowledge-worker 进程内），事件类型 `wiki.ingest/query_file/lint/reconcile/cancelled`；事件信封复用 §6.1 | §6.2 Stream 划分 |
| D9 | 三类操作落地 | ingest（仅更新受影响页，按来源依赖图增量）、query_file（显式「沉淀回答」触发）、lint（定期/人工检测过期/冲突/孤立/缺源/Schema 偏差，只产报告或候选不发布） | §7.5 首版三类操作 |
| D10 | 模块组织 | 新增 `internal/module/knowledge/wiki/`（service/ provider/ lint/ index/ handler/），复用 Phase 1 `knowledge-worker` Job dispatch | §3.1 目标目录；14 §5.2 job_type 扩展 |

---

## 1. 范围与依赖

### 1.1 本文档覆盖

落地设计文档 12 §16.3 的四项交付：
1. `wiki_spaces` / `wiki_pages` / `wiki_page_sources` / `wiki_maintenance_runs`（本文新增 `wiki_page_proposals`）表与 Schema Document、页面来源依赖、Maintenance Run。
2. 三类操作 ingest / query-file / lint；模型只返回受 JSON Schema 约束的 `PagePatch`。
3. `managed` / `locked` / `manual` 页面差异化写入策略：managed 提候选、locked 只产旁路建议、manual 不动；所有生成修订先进候选。
4. 确定性维护 index/log Document，复用现有 Document FTS/Qdrant 投影与审核 UI。

### 1.2 依赖（Phase 1，已落地）

本文档假设以下 Phase 0/1 基线已存在（已核对 migrations/013、014）：

- `knowledge_assets` / `knowledge_asset_versions`（013）+ `source_id` FK（014 回补）。
- `governance_profiles`（013，含 `legacy_migration` 系统行 014 补）。
- `knowledge_relations` / `review_requests` / `review_decisions` / `asset_projections`（014 七表）。
- `knowledge_jobs` / `outbox_events` / `outbox_deliveries`（013）。
- `knowledge-worker` 进程与 Job dispatch（14 §5.2：`source_sync` / `projection_build` / `asset_activate` / `reconcile_scan` / `legacy_backfill`）。
- CAS 版本激活机制（14 §7：`latest_requested_version_no` 单调栅栏 + `current_version_id` CAS）。
- `documents` / `document_versions`（003）：Wiki 页面正文落 `documents.content`（JSONB Block）+ `content_text`（FTS），版本落 `document_versions`。

### 1.3 非目标

- 不实现 CodeGraph / Memory / Skill 维护（Phase 3+）。
- 不替代现有 `rag-worker` 的切块/向量化；Wiki 页面投影复用 `rag-worker` Document 流水线。
- 不引入新的 `asset_type`；Wiki 页面是 `asset_type='document'` 的 Document Asset。
- 不在本阶段定 Schema Document 的默认内容（属产品/架构开放决策 §19.5，本文档给出 Schema Document 的结构契约，具体内容由 PM/架构共同产出）。

---

## 2. 数据架构

### 2.1 新增表：`wiki_spaces`

Wiki Space 是一组受同一 Schema Document 和维护策略约束的 Document Asset。一个 Space 指向其 Schema、index、log 三类 Document Asset，以及维护策略。

```sql
-- migrations/016_phase2_wiki_maintenance.up.sql
-- Phase 2：Wiki 增量维护（design-docs/12 §4.2、§7.5、§16.3，决策 D1）。
-- 依赖：013 knowledge_assets/versions、governance_profiles、knowledge_jobs；
--       014 review_requests/decisions、knowledge_relations、asset_projections；
--       003 documents/document_versions。Wiki 不新增 asset_type，页面复用
--       knowledge_assets(asset_type='document')。

-- Wiki Space：受同一 Schema 与维护策略约束的页面集合
CREATE TABLE wiki_spaces (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                VARCHAR(500) NOT NULL,
    -- Schema Document：描述页面类别、命名、引用和维护规则（§4.2）
    schema_asset_id     UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE RESTRICT,
    schema_version_id   UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE RESTRICT,
    -- 确定性维护 index/log Document Asset（§4.2，也属 document 资产）
    index_asset_id      UUID REFERENCES knowledge_assets(id) ON DELETE SET NULL,
    log_asset_id       UUID REFERENCES knowledge_assets(id) ON DELETE SET NULL,
    -- 治理与维护策略
    governance_profile_id UUID NOT NULL REFERENCES governance_profiles(id),
    maintenance_policy  JSONB NOT NULL DEFAULT '{}',  -- auto_approve_scope/lint_cron/拆分阈值
    status              VARCHAR(20) NOT NULL DEFAULT 'active', -- active|paused|archived
    created_by_type     VARCHAR(20) NOT NULL,
    created_by_id       UUID NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (status IN ('active','paused','archived')),
    -- Schema index/log 三类指向必须同 workspace（应用层校验 + 约束提示）
    -- （跨 workspace 校验在 service 层；DB 层 schema_asset_id 非空即 Schema 存在）
    UNIQUE (workspace_id, name)
);
CREATE INDEX idx_wiki_spaces_workspace ON wiki_spaces(workspace_id) WHERE status = 'active';
```

**不变量与约束说明**：
- `schema_asset_id` / `schema_version_id` 用 `ON DELETE RESTRICT`：Schema Document 不得在 Space 仍引用时删除；Schema 演进只能创建新版本（`schema_version_id` 可更新），旧版本保留可回溯。
- `index_asset_id` / `log_asset_id` 用 `ON DELETE SET NULL`：删除 index/log Document 不级联删 Space，但 Space 进入 `paused` 待重建（对账任务兜底，§12.3）。
- `maintenance_policy` JSONB 承载开放决策 §19.5 的参数：`auto_approve_scope`（可信自动批准范围）、`page_split_threshold`（页面拆分阈值）、`lint_cron`（lint 周期）。首版可空对象，由 PM/架构补内容。
- `status='archived'` 不级联硬删页面（§12.2 删除矩阵：页面仍是独立 Document）。

### 2.2 新增表：`wiki_pages`

页面是 Wiki Space 内的一个 Document Asset 的维护视图：记录 `page_key`、页面类别、自动化状态与过期原因。正文与版本仍由 `knowledge_assets` / `knowledge_asset_versions` 承载。

```sql
-- Wiki 页面维护视图（§4.2 wiki_pages）
-- 一个 wiki_page 行 = 一个 Document Asset 在某 Space 中的维护元数据。
-- 正文/版本/投影/审核全部走既有资产链路，本表只维护 page_key/类别/自动化状态。
CREATE TABLE wiki_pages (
    wiki_space_id       UUID NOT NULL REFERENCES wiki_spaces(id) ON DELETE CASCADE,
    document_asset_id   UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    page_key            TEXT NOT NULL,           -- 稳定键，Schema 约束的命名
    page_kind           VARCHAR(20) NOT NULL,   -- summary|entity|concept|comparison|synthesis|index|log
    automation_state    VARCHAR(20) NOT NULL DEFAULT 'manual', -- managed|locked|manual
    last_maintained_at  TIMESTAMPTZ,
    stale_reason        VARCHAR(30),            -- stale|conflict|orphan|missing_source|schema_drift|NULL
    stale_since         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (wiki_space_id, document_asset_id),
    UNIQUE (wiki_space_id, page_key),
    CHECK (page_kind IN ('summary','entity','concept','comparison','synthesis','index','log')),
    CHECK (automation_state IN ('managed','locked','manual')),
    -- locked/manual 页不走自动覆盖；stale_reason 仅在 lint 置位
    CHECK (
        stale_reason IS NULL OR
        stale_reason IN ('stale','conflict','orphan','missing_source','schema_drift')
    )
);
CREATE INDEX idx_wiki_pages_kind ON wiki_pages(wiki_space_id, page_kind);
CREATE INDEX idx_wiki_pages_stale ON wiki_pages(wiki_space_id, stale_reason)
    WHERE stale_reason IS NOT NULL;
CREATE INDEX idx_wiki_pages_automation ON wiki_pages(wiki_space_id, automation_state)
    WHERE automation_state = 'managed';
```

**不变量与约束说明**：
- `PRIMARY KEY(wiki_space_id, document_asset_id)`：一个 Document Asset 在一个 Space 中只有一行维护视图。
- `UNIQUE(wiki_space_id, page_key)`：`page_key` 在 Space 内唯一，由 Schema Document 约束命名规则。
- `automation_state` 决定写入策略（§4 差异化策略）。`locked` / `manual` 页：维护器产出的 patch 只能进旁路建议（`wiki_page_proposals`，§2.4），不创建覆盖性 `knowledge_asset_versions` 候选。
- `stale_reason` 由 lint 置位（§5.3），由 ingest 成功激活后清除。`stale_since` 支持过期时长排序与 lint 增量游标。
- 不在 `wiki_pages` 存正文/版本：`document_asset_id → knowledge_assets.current_version_id → knowledge_asset_versions.native_document_version_id → document_versions.content` 是正文唯一路径，避免双写。

### 2.3 新增表：`wiki_page_sources`

每个生成页版本必须锚定实际读取的来源版本；仅记 Source ID 或最新版本不足以复现结论（§4.2）。

```sql
-- 页面来源依赖：生成某页版本时实际读取的来源版本集合（§4.2 wiki_page_sources）
-- page_asset_version_id 指向 knowledge_asset_versions.id（生成的页候选版本）
-- source_asset_version_id 指向 knowledge_asset_versions.id（被读取的来源版本）
CREATE TABLE wiki_page_sources (
    page_asset_version_id  UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    source_asset_id        UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    source_asset_version_id UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    contribution_hash     TEXT NOT NULL,        -- 该来源对本次生成的贡献指纹（去重/冲突检测）
    relation_kind          VARCHAR(30) NOT NULL, -- derived_from|explains|contradicts|supersedes
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (page_asset_version_id, source_asset_id, source_asset_version_id),
    CHECK (relation_kind IN ('derived_from','explains','contradicts','supersedes'))
);
CREATE INDEX idx_wiki_sources_page ON wiki_page_sources(page_asset_version_id);
CREATE INDEX idx_wiki_sources_source ON wiki_page_sources(source_asset_id, source_asset_version_id);
```

**不变量与约束说明**：
- `PRIMARY KEY(page_asset_version_id, source_asset_id, source_asset_version_id)`：同一页版本对同一来源版本的锚定唯一。
- `contribution_hash`：用于检测重复贡献与冲突（同来源不同版本对同一 `page_key` 的结论冲突）。
- 删除来源版本不级联删已发布页面历史：`ON DELETE CASCADE` 只清依赖锚点，对账任务（§12.3）将相关页标 `stale` 并触发 lint/修订候选（§12.2 删除矩阵）。
- `wiki_page_sources` 与 `knowledge_relations` 互补：前者记录「页版本 → 来源版本」的细粒度读取依赖，后者记录资产级关系（`derived_from/contradicts/supersedes`），后者由 Mora 在候选激活时落库（§10.4：关系落库由 Mora 完成）。

### 2.4 新增表：`wiki_maintenance_runs` 与 `wiki_page_proposals`

Maintenance Run 是一次维护调度的不可变快照；PagePatch 落为 proposal 行，经审核/可信策略批准后逐页 CAS 激活。

```sql
-- Maintenance Run：一次维护调度的不可变快照（§4.2 wiki_maintenance_runs）
CREATE TABLE wiki_maintenance_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wiki_space_id       UUID NOT NULL REFERENCES wiki_spaces(id) ON DELETE CASCADE,
    trigger_type        VARCHAR(20) NOT NULL,   -- ingest|query_file|lint|manual
    -- 幂等键：相同输入集合 + Schema + 模型 + Prompt 的重试保持幂等（§7.5）
    schema_version_id   UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE RESTRICT,
    input_set_hash       TEXT NOT NULL,        -- 规范化输入版本集合的 hash
    model_revision       TEXT NOT NULL,
    prompt_revision      TEXT NOT NULL,
    requested_by_type    VARCHAR(20) NOT NULL,
    requested_by_id      UUID NOT NULL,
    -- 显式 query_file 的回答沉淀来源（仅 trigger=query_file 非空）
    answer_ref           JSONB,                -- 不可执行引用：{asset_id, version_id, excerpt_hash}
    status               VARCHAR(20) NOT NULL DEFAULT 'queued',
    -- queued|analyzing|proposing|awaiting_review|applied|failed|cancelled
    proposal_manifest    JSONB,               -- {page_keys[], expected_versions{}, patch_count}
    idempotency_key      TEXT NOT NULL UNIQUE, -- = input_set_hash + schema + model + prompt（规范形式）
    started_at           TIMESTAMPTZ,
    finished_at          TIMESTAMPTZ,
    error_code           VARCHAR(60),
    error_detail_redacted TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (trigger_type IN ('ingest','query_file','lint','manual')),
    CHECK (status IN ('queued','analyzing','proposing','awaiting_review','applied','failed','cancelled')),
    CHECK (NOT (trigger_type = 'query_file' AND answer_ref IS NULL))
);
CREATE INDEX idx_wiki_runs_space ON wiki_maintenance_runs(wiki_space_id, created_at DESC);
CREATE INDEX idx_wiki_runs_status ON wiki_maintenance_runs(status, started_at)
    WHERE status IN ('queued','analyzing','proposing');
CREATE INDEX idx_wiki_runs_idempotency ON wiki_maintenance_runs(wiki_space_id, idempotency_key);

-- 页面候选修订：维护器产出的 PagePatch 落为此表行（§7.5，决策 D3/D4）
-- 一行 = 一个 page_key 的一个候选 patch；经审核后逐页 CAS 激活。
CREATE TABLE wiki_page_proposals (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID NOT NULL REFERENCES wiki_maintenance_runs(id) ON DELETE CASCADE,
    wiki_space_id       UUID NOT NULL REFERENCES wiki_spaces(id) ON DELETE CASCADE,
    page_key            TEXT NOT NULL,
    page_asset_id       UUID REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    -- 期望的当前版本（CAS 前置条件）；NULL=新建页
    expected_version_id UUID REFERENCES knowledge_asset_versions(id),
    -- 生成的候选版本（通过后创建；locked/manual 页可能为 NULL=旁路建议）
    proposed_version_id UUID REFERENCES knowledge_asset_versions(id) ON DELETE SET NULL,
    action              VARCHAR(20) NOT NULL,  -- create|update|link|contradiction|stale
    is_bypass            BOOLEAN NOT NULL DEFAULT false, -- locked/manual 页的旁路建议=true
    content_hash        TEXT NOT NULL,        -- 候选正文 hash
    relation_suggestions JSONB NOT NULL DEFAULT '[]',  -- [{kind, to_asset_id, to_version_id}]
    status              VARCHAR(20) NOT NULL DEFAULT 'proposed',
    -- proposed|approved|rejected|superseded|applied|failed
    review_request_id   UUID REFERENCES review_requests(id) ON DELETE SET NULL,
    applied_at          TIMESTAMPTZ,
    error_detail_redacted TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (action IN ('create','update','link','contradiction','stale')),
    CHECK (status IN ('proposed','approved','rejected','superseded','applied','failed')),
    CHECK (NOT (is_bypass = false AND proposed_version_id IS NULL AND action IN ('create','update')))
);
CREATE INDEX idx_wiki_proposals_run ON wiki_page_proposals(run_id);
CREATE INDEX idx_wiki_proposals_page ON wiki_page_proposals(wiki_space_id, page_key, created_at DESC);
CREATE INDEX idx_wiki_proposals_pending ON wiki_page_proposals(status)
    WHERE status IN ('proposed','approved');
```

**不变量与约束说明**：
- `wiki_maintenance_runs.idempotency_key UNIQUE` + `wiki_maintenance_runs(wiki_space_id, idempotency_key)`：相同 `input_set_hash + schema_version_id + model_revision + prompt_revision` 的重试，同一 Space 内幂等（§7.5）。
- `wiki_maintenance_runs` 不可变快照：`schema_version_id` / `input_set_hash` / `model_revision` / `prompt_revision` 在 Run 创建时固化，后续编辑 Space 策略不影响已排队 Run。
- `wiki_page_proposals.is_bypass`：`locked` / `manual` 页的 patch `is_bypass=true`，`proposed_version_id` 可为 NULL（旁路建议不创建覆盖性候选，决策 D3）。`managed` 页 `is_bypass=false`，`proposed_version_id` 在审核通过后创建候选 `knowledge_asset_versions`。
- `expected_version_id`：CAS 前置条件（§4 逐页 CAS）。旧版本晚完成时 `expected_version_id` 不匹配 → CAS 失败 → 该页标 `superseded`/`failed`，不影响其他页（决策 D4）。
- `review_request_id`：复用 Phase 1 审核链路（014 `review_requests`/`review_decisions`）。可信自动批准范围的 managed 页可走 `auto_publish` 策略直接 `approved`（§4.2 治理），但仍逐页 CAS 激活。

### 2.5 与现有表的关系图（ER 要点）

```
workspaces 1───* wiki_spaces *───1 knowledge_assets(schema)
                              ├──1 knowledge_assets(index/log)
                              └──1 governance_profiles

wiki_spaces 1───* wiki_pages *───1 knowledge_assets(page document)
wiki_pages.page_key UNIQUE(wiki_space_id, page_key)

wiki_maintenance_runs *───1 wiki_spaces
wiki_maintenance_runs.schema_version_id ──▶ knowledge_asset_versions

wiki_page_proposals *───1 wiki_maintenance_runs
wiki_page_proposals *───1 wiki_spaces
wiki_page_proposals.proposed_version_id ──▶ knowledge_asset_versions
wiki_page_proposals.review_request_id ──▶ review_requests

wiki_page_sources.page_asset_version_id ──▶ knowledge_asset_versions(generated page)
wiki_page_sources.source_asset_version_id ──▶ knowledge_asset_versions(source)
```

### 2.6 回滚迁移

`migrations/016_phase2_wiki_maintenance.down.sql`：按反向依赖顺序 `DROP TABLE wiki_page_proposals, wiki_page_sources, wiki_maintenance_runs, wiki_pages, wiki_spaces;`。Wiki 页面正文仍存于 `documents`，回滚只删维护元数据，不删正文。

---

## 3. 模块与代码组织

### 3.1 目标目录（§3.1）

```
internal/module/knowledge/
  wiki/                       # Phase 2：Document 页增量维护、依赖、lint 与候选修订
    service/                  # Wiki Space/Run 编排、逐页 CAS、差异化写入策略
    provider/                 # WikiMaintenanceProvider 端口 + 适配（外部模型/本地实现）
    lint/                     # 过期/冲突/孤立/缺源/Schema 偏差检测规则
    index/                    # 确定性 index/log Document 重建
    handler/                  # REST 控制面（wiki-spaces / maintenance-runs / :lint）
```

### 3.2 依赖规则（§3.2 扩展）

- `wiki` 模块依赖 `knowledge/asset`（版本创建、CAS 激活）、`knowledge/governance`（审核/发布）、`knowledge/source`（来源版本读取）、`platform/rbac`（授权裁剪）、`platform/authz`（capability 校验）。
- `wiki` 不直接写 Qdrant / FTS：Wiki 页面作为 `documents` 行写入后，复用 `rag-worker` 既有 Document 流水线（05 §3），由 `asset_projections` 跟踪投影状态。
- `wiki/provider` 只实现 `WikiMaintenanceProvider` 端口（§4.1），外部模型实现可替换；Provider 不得反向依赖 `wiki/service`。

### 3.3 knowledge-worker Job dispatch 扩展（14 §5.2）

新增 job_type，复用 Phase 1 的 `knowledge_jobs` 与 dedupe_key 规约：

| job_type | dedupe_key 形态 | 处理 | 产出 |
|---|---|---|---|
| `wiki_maintain` | `wiki:{space_id}:{trigger}:{input_set_hash}` | 调 WikiMaintenanceProvider（ingest/query_file/lint）产 PagePatch[] | `wiki_page_proposals` 行 |
| `wiki_proposal_apply` | `wiki_apply:{proposal_id}` | 逐页 CAS 激活候选 `knowledge_asset_versions` | `knowledge_assets.current_version_id` 切换 |
| `wiki_index_rebuild` | `wiki_idx:{space_id}:{index_version_hash}` | 确定性重建 index Document | `documents` 行 + 投影 job |
| `wiki_lint_scan` | `wiki_lint:{space_id}:{cursor}` | 增量 lint 扫描（§5.3） | `wiki_pages.stale_reason` + lint 报告 |

---

## 4. Maintenance Provider 契约与差异化写入策略

### 4.1 Provider 端口（§10.4）

```go
// Package provider 定义 Wiki 维护器端口（design-docs/12 §10.4）。
// Provider 只接收 Mora 已授权并按预算裁剪的页面、来源内容和不可执行 Schema；
// 不自行读取数据库、对象存储、URL 或 Git。返回值必须通过 JSON Schema，且只表达
// 候选 patch、引用和诊断。路径规范化、expected-version CAS、关系落库、审核和发布
// 全部由 Mora 完成。
package provider

type Capability struct {
    WorkspaceID  uuid.UUID
    AuthzRevision int64
    MaxReadBytes  int
    MaxReadPages  int
}

type WikiIngestRequest struct {
    WikiSpaceID   uuid.UUID
    Schema        json.RawMessage // 不可执行 Schema Document 快照
    AffectedPages []PageRef       // 受来源变更影响的页面及摘要
    SourceVersions []SourceVersionRef // 已授权裁剪的来源版本内容
}

type WikiAnswerRequest struct {
    WikiSpaceID  uuid.UUID
    Schema       json.RawMessage
    PageKey      string
    AnswerRef    json.RawMessage // 不可执行引用：{asset_id, version_id, excerpt_hash}
    SourceVersions []SourceVersionRef
}

type WikiLintRequest struct {
    WikiSpaceID uuid.UUID
    Schema      json.RawMessage
    PagesCursor json.RawMessage // 增量游标（§5.3）
    CheckKinds  []string       // stale|conflict|orphan|missing_source|schema_drift
}

// PagePatch 受 JSON Schema 约束（§4.2），必须携带目标 page_key、expected current version、
// 完整来源版本列表、变更类型、候选正文 hash 和关系建议。
type PagePatch struct {
    PageKey            string             `json:"page_key"`
    ExpectedVersionID  *uuid.UUID         `json:"expected_version_id,omitempty"`
    Action             string             `json:"action"` // create|update|link|contradiction|stale
    ContentHash        string             `json:"content_hash"`
    SourceVersions     []SourceVersionRef `json:"source_versions"`
    RelationSuggestions []RelationSuggestion `json:"relation_suggestions"`
}

type RelationSuggestion struct {
    Kind        string `json:"kind"` // derived_from|explains|contradicts|supersedes
    ToAssetID   uuid.UUID `json:"to_asset_id"`
    ToVersionID *uuid.UUID `json:"to_version_id,omitempty"`
}

type WikiLintReport struct {
    Findings []LintFinding `json:"findings"`
}

type LintFinding struct {
    PageKey     string `json:"page_key"`
    Reason      string `json:"reason"` // stale|conflict|orphan|missing_source|schema_drift
    Detail      json.RawMessage `json:"detail"`
    Suggestion  *PagePatch `json:"suggestion,omitempty"` // 可选修订候选
}

type WikiMaintenanceProvider interface {
    ProposeIngest(ctx context.Context, cap Capability, req WikiIngestRequest) ([]PagePatch, error)
    ProposeAnswer(ctx context.Context, cap Capability, req WikiAnswerRequest) ([]PagePatch, error)
    Lint(ctx context.Context, cap Capability, req WikiLintRequest) (WikiLintReport, error)
    Health(ctx context.Context) error
}
```

### 4.2 PagePatch JSON Schema（校验门禁）

模型返回的每个 `PagePatch` 必须通过以下 JSON Schema，未通过的 patch 整条 Run 标 `failed`，不落任何候选：

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "PagePatch",
  "type": "object",
  "required": ["page_key", "action", "content_hash", "source_versions", "relation_suggestions"],
  "properties": {
    "page_key":        { "type": "string", "minLength": 1, "maxLength": 256 },
    "expected_version_id": { "type": ["string", "null"], "format": "uuid" },
    "action":          { "enum": ["create", "update", "link", "contradiction", "stale"] },
    "content_hash":    { "type": "string", "pattern": "^[a-f0-9]{64}$" },
    "source_versions": {
      "type": "array", "minItems": 1,
      "items": {
        "type": "object",
        "required": ["source_asset_id", "source_asset_version_id"],
        "properties": {
          "source_asset_id":         { "type": "string", "format": "uuid" },
          "source_asset_version_id": { "type": "string", "format": "uuid" },
          "contribution_hash":        { "type": "string", "pattern": "^[a-f0-9]{64}$" }
        }
      }
    },
    "relation_suggestions": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["kind", "to_asset_id"],
        "properties": {
          "kind":          { "enum": ["derived_from", "explains", "contradicts", "supersedes"] },
          "to_asset_id":   { "type": "string", "format": "uuid" },
          "to_version_id": { "type": ["string", "null"], "format": "uuid" }
        }
      }
    }
  }
}
```

**校验要点**：
- `source_versions` `minItems:1`：每个生成页版本必须锚定至少一个来源版本（不变量 10）。
- `content_hash` 强制 SHA-256：用于幂等检测（同输入集合同 hash = 幂等命中）与 CAS 前置。
- `page_key` 长度限制：防模型注入超长键。

### 4.3 三类操作流程（§7.5）

**ingest（来源到达，仅更新受影响页）**：

```
来源版本发布（source_sync_runs.status='ready'）
  → 事件 wiki.ingest 投递 wiki_events（input_set = 受影响的来源版本集合）
  → knowledge-worker 创建 wiki_maintain job
  → service 计算受影响 page_key（按 wiki_page_sources 反查依赖图）
  → 调 ProposeIngest，传入受影响页面摘要 + 已授权来源版本
  → 校验 PagePatch[] JSON Schema
  → 对每个 patch：按 automation_state 分流（§4.4）
  → managed：创建 wiki_page_proposals(proposed_version_id=候选版本, is_bypass=false)
  → locked/manual：创建 wiki_page_proposals(is_bypass=true, proposed_version_id=NULL)
  → 人工审核或可信策略批准
  → 逐页 CAS 激活（§4.4）
  → 确定性重建 index Document，追加 log Document
  → 触发既有 FTS/Qdrant 投影（documents 写入 → rag-worker）
```

**query_file（显式「沉淀回答」）**：

```
用户/Agent 显式选择「沉淀回答」
  → POST /api/v1/wiki-spaces/{id}/maintenance-runs (trigger=query_file, answer_ref)
  → 创建 wiki_maintain job（input_set = answer 引用的来源版本）
  → 调 ProposeAnswer，传入 page_key + answer_ref + 已授权来源版本
  → 校验 PagePatch[] JSON Schema
  → 同 ingest 分流与激活流程
普通查询不自动写 Wiki（§11.3：wiki_page_propose 只在明确要求时创建候选）
```

**lint（定期/人工检测）**：

```
lint_cron 触发或人工 POST :lint
  → 创建 wiki_lint_scan job（增量游标，非全量扫描，§18）
  → 调 Lint，传入 Schema + 游标 + check_kinds
  → 对每个 finding：置 wiki_pages.stale_reason
  → 只产报告或修订候选（Suggestion 可选），不直接发布
  → Suggestion 走 ingest 同样的分流与激活流程
```

### 4.4 差异化写入策略（managed / locked / manual）

| automation_state | 维护器可产 patch | 候选版本创建 | 激活策略 |
|---|---|---|---|
| `managed` | create/update/link/contradiction/stale | 创建候选 `knowledge_asset_versions(content_origin='generated')` | 审核通过或可信策略 → 逐页 CAS 激活 |
| `locked` | contradiction/stale（旁路建议） | **不创建覆盖性候选**；`is_bypass=true`，`proposed_version_id=NULL` | 不激活；旁路建议进审核 UI 供人工判断 |
| `manual` | 不动（不接收 patch） | 不创建候选 | 不激活；lint 可标 `stale_reason` 但不自动改 |

**locked 页保护实现要点**（决策 D3，门禁「locked 页自动覆盖为 0」）：
1. service 在调 Provider 前，按 `automation_state` 过滤传入页集合：`locked` 页只传摘要（page_key + 当前版本摘要），不传正文，不允许 `update`/`create` action 的 patch 通过。
2. Provider 返回的 patch 若对 `locked` 页含 `update`/`create` action → schema 校验阶段拒绝 → Run 标 `failed`（或降级为旁路建议）。
3. 即使绕过校验，CAS 激活层对 `locked` 页硬拒：`wiki_page_proposals` 对 `automation_state='locked'` 且 `is_bypass=false` 的行不存在（约束 + service 双保险）。
4. 审计事件 `wiki.lock` 记录所有 locked 页的旁路建议（§13.4）。

### 4.5 逐页 CAS 激活（决策 D4）

沿用 Phase 1 版本原子切换（14 §7），但限定为逐页：

```sql
-- 逐页 CAS：expected_version_id 必须匹配 assets.current_version_id
UPDATE knowledge_assets a
SET current_version_id = $1,
    latest_requested_version_no = $2,
    updated_at = now()
FROM wiki_page_proposals p
WHERE p.id = $3
  AND p.page_asset_id = a.id
  AND p.status = 'approved'
  AND p.is_bypass = false
  AND a.current_version_id IS NOT DISTINCT FROM p.expected_version_id  -- CAS 前置
RETURNING a.id, p.id;
```

- `latest_requested_version_no` 单调栅栏：旧 Run 晚完成时 CAS 失败（`expected_version_id` 不匹配），该页标 `superseded`，不覆盖新版本。
- 逐页独立：一页 CAS 失败不影响其他页（§12.1，门禁「部分失败不替换最后已发布页面」）。
- 激活成功后 `wiki_page_proposals.status='applied'`，`wiki_pages.last_maintained_at=now()`，`stale_reason=NULL`。

---

## 5. 确定性 index/log 与投影复用

### 5.1 确定性 index Document（决策 D6）

index Document 是 Space 的目录页，由已发布页面确定性重建，不由模型自由改写：

- **确定性排序键**：`(page_kind, page_key)` 稳定排序，页面顺序不依赖生成时间。
- **稳定哈希**：`index_version_hash = sha256(sorted([(page_key, current_version.content_hash) for published pages]))`。相同已发布页面集合 → 相同 hash → index 版本幂等。
- **重建触发**：任一页 CAS 激活成功后，投递 `wiki_index_rebuild` job（dedupe_key=`wiki_idx:{space_id}:{index_version_hash}`，幂等）。
- **index 也是 Document Asset**：`knowledge_assets(asset_type='document', native_document_id=index_document_id)`，其版本走 `knowledge_asset_versions(content_origin='system')`，不经审核直接激活（系统维护）。

### 5.2 确定性 log Document

log Document 由 Run/Decision 事件追加，不由模型自由改写：

- **追加语义**：每条 Run 完成或 Decision 产生，向 log Document 追加一条结构化记录（`run_id`、`trigger`、`page_keys`、`decision`、`actor`、`timestamp`）。
- **不可改写**：log 版本的 `content` 是只追加的 JSONB 数组；重建只追加，不原地修改历史条目。
- **log 也是 Document Asset**：`content_origin='system'`，同样复用 FTS/Qdrant 投影。

### 5.3 投影复用（决策 D7）

Wiki 页面作为 `documents` 行写入后，投影复用既有链路：

| 投影 | 复用机制 | 说明 |
|---|---|---|
| FTS | `documents.content_text` + `idx_documents_fts` | Wiki 页写 `documents` 时同步写 `content_text`，GIN 索引免费复用 |
| 向量 | `mora_chunks_*` + 确定 point id（`uuid5`） | Wiki 页 `document.create`/`update` 事件走 `doc_events`，rag-worker 切块向量化 |
| 投影状态 | `asset_projections(projection_kind='fts'/'vector')` | 复用 Phase 1 投影跟踪；非阻塞投影未就绪降级不覆盖旧版本（14 §7） |
| RBAC | `visible_to` payload + FTS SQL 过滤 | Wiki 页权限走既有 `permission.change` 重算（05 §4.3.3） |

---

## 6. 事件与异步处理

### 6.1 wiki_events Stream（§6.2）

```
stream: wiki_events
group:  wiki_maintenance   (knowledge-worker 进程内消费组)
events: wiki.ingest | wiki.query_file | wiki.lint | wiki.reconcile | wiki.cancelled
```

事件信封复用 §6.1 通用格式：

```json
{
  "event_id": "uuid",
  "event_type": "wiki.ingest",
  "wiki_space_id": "uuid",
  "workspace_id": "uuid",
  "trigger": "ingest",
  "input_set_hash": "sha256...",
  "schema_version_id": "uuid",
  "model_revision": "...",
  "prompt_revision": "...",
  "requested_by_type": "agent",
  "requested_by_id": "uuid",
  "timestamp": "2026-08-15T08:00:00Z"
}
```

### 6.2 事务一致性

- Mora API 在 PG 事务内完成 `wiki_maintenance_runs` 创建 + `wiki_page_proposals` 落库 + Outbox 事件投递（`pgx.AfterCommit`）。
- 投递失败记录到 `outbox_events(published_at IS NULL)`，补偿扫描器重投（复用 05 §2.5）。
- 消费组 ACK 语义：knowledge-worker 完成全部 patch 落库后才 `XACK`；崩溃后未 ACK 消息由 `XAUTOCLAIM`（idle > 60s）认领。
- 幂等：以 `event_id` 在 Valkey SET 去重（TTL 24h）；Run 级 `idempotency_key` UNIQUE 双保险。

---

## 7. API 契约

### 7.1 REST 控制面（§11.1）

新增 `api/wiki.yaml`，沿用 `api/rag.yaml` 的 OpenAPI 3.0.3 风格与 §1 通用约定（BearerAuth、`{code,data,message}` 信封、§1.4 错误码）。

```yaml
openapi: 3.0.3
info:
  title: Mora Wiki Maintenance API
  version: "1.0"
  description: |
    Wiki 增量维护控制面（design-docs/12 §11.1、§16.3 / YS-96）。
    维护器只提候选、不直接写生产文档；locked 页只产旁路建议。
    RBAC 为硬约束：未授权 Wiki Space 不返回、不计入 total（存在性不泄露）。
security:
  - BearerAuth: []
paths:
  /api/v1/workspaces/{workspace_id}/wiki-spaces:
    get:
      tags: [Wiki]
      summary: 列出 Wiki Space
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: page, in: query, schema: { type: integer, default: 1 } }
        - { name: page_size, in: query, schema: { type: integer, default: 20 } }
      responses:
        "200":
          description: Wiki Space 列表
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/WikiSpace' }
                      total: { type: integer }
    post:
      tags: [Wiki]
      summary: 创建 Wiki Space
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name, schema_asset_id, schema_version_id, governance_profile_id]
              properties:
                name: { type: string }
                schema_asset_id: { type: string, format: uuid }
                schema_version_id: { type: string, format: uuid }
                governance_profile_id: { type: string, format: uuid }
                maintenance_policy: { type: object }
      responses:
        "201":
          description: 创建成功
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/WikiSpace' }

  /api/v1/wiki-spaces/{id}:
    get:
      tags: [Wiki]
      summary: 查询 Wiki Space 状态（含维护状态、页面统计）
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        "200":
          description: Wiki Space 详情
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/WikiSpace' }

  /api/v1/wiki-spaces/{id}/maintenance-runs:
    get:
      tags: [Wiki]
      summary: 列出维护 Run
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: status, in: query, schema: { type: string, enum: [queued, analyzing, proposing, awaiting_review, applied, failed, cancelled] } }
        - { name: page, in: query, schema: { type: integer, default: 1 } }
        - { name: page_size, in: query, schema: { type: integer, default: 20 } }
      responses:
        "200":
          description: Run 列表
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/MaintenanceRun' }
                      total: { type: integer }
    post:
      tags: [Wiki]
      summary: 触发维护 Run（trigger=query_file 显式沉淀回答）
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [trigger]
              properties:
                trigger: { type: string, enum: [ingest, query_file, lint, manual] }
                page_key: { type: string, description: 仅 query_file 必填 }
                answer_ref: { type: object, description: 仅 query_file 必填，不可执行引用 }
                check_kinds: { type: array, items: { type: string }, description: 仅 lint }
      responses:
        "201":
          description: Run 已创建
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/MaintenanceRun' }

  # Wire path is {id}/lint, not AIP-190's {id}:lint: Gin v1.12's route tree
  # parses ':id:lint' as two params in one segment → startup panic. See
  # api/wiki.yaml for the same note; the handler reads c.Param("id") unchanged.
  /api/v1/wiki-spaces/{id}/lint:
    post:
      tags: [Wiki]
      summary: 触发 lint 扫描
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                check_kinds: { type: array, items: { type: string, enum: [stale, conflict, orphan, missing_source, schema_drift] } }
      responses:
        "202":
          description: lint job 已排队
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/MaintenanceRun' }

  /api/v1/wiki-spaces/{id}/pages/{page_key}/proposals:
    get:
      tags: [Wiki]
      summary: 列出页面候选修订（审核 UI 用）
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: page_key, in: path, required: true, schema: { type: string } }
        - { name: status, in: query, schema: { type: string, enum: [proposed, approved, rejected, superseded, applied, failed] } }
      responses:
        "200":
          description: 候选列表
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/PageProposal' }

  /api/v1/wiki-spaces/{id}/proposals/{proposal_id}:
    post:
      tags: [Wiki]
      summary: 审核页面候选（approve/reject；逐页 CAS 激活）
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: proposal_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [decision]
              properties:
                decision: { type: string, enum: [approve, reject] }
                rationale: { type: string }
      responses:
        "200":
          description: 审核结果
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/PageProposal' }

components:
  schemas:
    WikiSpace:
      type: object
      properties:
        id: { type: string, format: uuid }
        workspace_id: { type: string, format: uuid }
        name: { type: string }
        schema_asset_id: { type: string, format: uuid }
        schema_version_id: { type: string, format: uuid }
        index_asset_id: { type: string, format: uuid }
        log_asset_id: { type: string, format: uuid }
        governance_profile_id: { type: string, format: uuid }
        maintenance_policy: { type: object }
        status: { type: string, enum: [active, paused, archived] }
    MaintenanceRun:
      type: object
      properties:
        id: { type: string, format: uuid }
        wiki_space_id: { type: string, format: uuid }
        trigger_type: { type: string, enum: [ingest, query_file, lint, manual] }
        schema_version_id: { type: string, format: uuid }
        input_set_hash: { type: string }
        model_revision: { type: string }
        prompt_revision: { type: string }
        status: { type: string, enum: [queued, analyzing, proposing, awaiting_review, applied, failed, cancelled] }
        proposal_manifest: { type: object }
        started_at: { type: string, format: date-time }
        finished_at: { type: string, format: date-time }
    PageProposal:
      type: object
      properties:
        id: { type: string, format: uuid }
        run_id: { type: string, format: uuid }
        page_key: { type: string }
        page_asset_id: { type: string, format: uuid }
        expected_version_id: { type: string, format: uuid, nullable: true }
        proposed_version_id: { type: string, format: uuid, nullable: true }
        action: { type: string, enum: [create, update, link, contradiction, stale] }
        is_bypass: { type: boolean }
        content_hash: { type: string }
        status: { type: string, enum: [proposed, approved, rejected, superseded, applied, failed] }
```

### 7.2 内部 API（§11.2）

```
POST /internal/v1/wiki-spaces/{id}:propose-page   # Provider 回调（受 capability 校验）
GET  /internal/v1/wiki-spaces/{id}/status          # knowledge-worker 内部状态查询
```

内部 API 走短期 delegated context（Phase 0 的 `delegated_sessions` + `authorization_decisions`），不依赖共享 `INTERNAL_SERVICE_TOKEN`。

### 7.3 MCP 工具（§11.3）

- `wiki_status`：返回 Space 目录、维护状态、可见 lint 报告。只读，受 RBAC 约束。
- `wiki_page_propose`：用户/Agent 明确要求沉淀回答时创建候选，不直接发布。普通查询不触发。
- 管理型操作（运行 lint、删除投影、发布页面）不进入默认 Agent 工具集。

---

## 8. 安全架构

### 8.1 Prompt injection 防护（门禁）

- **输入集合由 Mora 授权裁剪**：Provider 调用前，service 按 `authz_revision` + RBAC 计算可见来源版本集合，传入 Provider 的内容已裁剪（§10.4）。
- **不可执行 Schema**：Schema Document 传给 Provider 时剥离可执行部分，只留页面类别/命名/引用规则。
- **页面正文指令不扩大读取范围**：Provider 不因页面正文中的指令读取额外内容；`source_versions` 集合在调用前固化，Provider 无权扩展（决策 D2）。
- **输出受 JSON Schema 约束**：PagePatch 校验失败 → Run 标 `failed`，不落候选（§4.2）。
- **locked 页内容不传正文**：§4.4 第 1 点，locked 页只传摘要，防模型基于正文改写。

### 8.2 存在性不泄露

- Wiki Space / 页面 / 候选列表查询均经 RBAC 过滤：未授权 Space 不返回、不计入 total（复用既有 `rbac_visible` 与 `visible_to`）。
- 错误码统一 40300（无权限）不区分「不存在」与「无权」，不泄露 Space 存在性。

### 8.3 审计事件（§13.4）

`wiki.ingest/propose/lint/review/apply/lock` 全部记 `audit_logs`（006），含 actor、wiki_space_id、page_key、decision、run_id。

---

## 9. 验收门禁对应（§16.3）

| 门禁 | 实现位置 | 验证方式 |
|---|---|---|
| 每个生成页版本可回溯全部输入版本与生成配置 | `wiki_page_sources`（来源版本） + `knowledge_asset_versions.generation_ref`（model/prompt/schema revision + input_set_hash + run_id） | 查询：给定页版本 → 列出全部 source_asset_version_id + generation_ref |
| locked 页被自动覆盖为 0 | §4.4 locked 页保护（schema 校验拒 update/create + CAS 层硬拒 + 约束） | 测试：对 locked 页投 update patch → Run failed/旁路，`current_version_id` 不变 |
| 相同输入集合的重复 Run 幂等 | Run `idempotency_key` UNIQUE + 页 `dedupe_key` + `input_set_hash` | 测试：同 input_set_hash 重跑 → 命中既有 Run/proposal，不产生新候选 |
| 部分失败不替换最后已发布页面 | 逐页 CAS（§4.5），失败页标 `failed`/`superseded` | 测试：多页 Run 中一页 CAS 失败 → 其他页正常激活，失败页 `current_version_id` 不变 |
| lint 可发现预置的过期/冲突/孤立/缺源/Schema 偏差样例 | §5 lint 规则 + `stale_reason` | 测试：预置 5 类样例 → lint 报告全部命中 |

---

## 10. 交付清单与角色分工

### 10.1 交付物（架构层，本文档）

- 迁移脚本 `migrations/016_phase2_wiki_maintenance.up/down.sql`（§2）。
- Provider 端口与 PagePatch JSON Schema（§4.1/§4.2）。
- REST 契约 `api/wiki.yaml`（§7.1）。
- 事件信封与 Stream 划分（§6）。
- 模块目录与 Job dispatch 扩展（§3）。

### 10.2 角色分工

- **架构师（本文档）**：表结构、Provider 契约、CAS 策略、事件流、API 契约、投影复用方案。
- **后端研发**：实现 `wiki/service`、`wiki/provider`、`wiki/lint`、`wiki/index`、`wiki/handler`；接入 knowledge-worker Job dispatch；实现逐页 CAS、JSON Schema 校验、locked 页保护。
- **产品经理**：Wiki Space 默认 Schema Document 内容、可信自动批准范围（`auto_approve_scope`）、页面拆分阈值（`page_split_threshold`）、lint 周期（`lint_cron`）——开放决策 §19.5。
- **设计师/前端**：审核与页面候选 UI（`/proposals` 列表 + approve/reject 交互）。
- **测试工程师**：lint 样例（过期/冲突/孤立/缺源/Schema 偏差）、幂等测试、locked 分支测试、逐页 CAS 部分失败测试。

---

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| Provider 输出绕过 JSON Schema | 双层校验：Provider 适配层校验 + service 落库前校验；失败 Run 标 failed 不落候选 |
| locked 页被覆盖 | 三重保险：schema 校验拒 action + CAS 层硬拒 is_bypass=false + 审计 wiki.lock |
| Run 全局事务导致部分失败回滚 | 非全局事务，逐页 CAS 独立原子（§4.5，§12.1） |
| 来源版本删除导致已发布页失依 | `wiki_page_sources` ON DELETE CASCADE 清锚点 + 对账任务标 stale + 触发 lint（§12.2/§12.3） |
| lint 全量扫描性能 | 增量游标（§5.3，§18：lint 使用增量游标而非每次全量扫描） |
| index 重建抖动 | 稳定哈希幂等 + dedupe_key 去重，相同已发布集合不重建 |

---

> 本文档为 YS-96 Phase 2 架构层交付物，与 design-docs/12 §16.3 门禁一致。迁移脚本与 Provider/REST 契约可直接交付研发实现。Wiki Space 默认 Schema 内容、可信自动批准范围、拆分阈值、lint 周期属开放决策（§19.5），待 PM/架构共同产出后补入 §2.1 `maintenance_policy`。
