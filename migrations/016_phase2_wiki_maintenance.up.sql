-- 016_phase2_wiki_maintenance.up.sql
-- Phase 2：Wiki 增量维护（design-docs/16 §2，决策 D1；12 §4.2/§7.5/§16.3）。
-- 在 Phase 0/1 基线之上补齐：wiki_spaces / wiki_pages / wiki_page_sources /
-- wiki_maintenance_runs / wiki_page_proposals 五表。Wiki 不新增 asset_type，页面
-- 复用 knowledge_assets(asset_type='document')；正文走 documents/document_versions。
--
-- 依赖：013 knowledge_assets/versions、governance_profiles、knowledge_jobs、outbox_events；
--       014 review_requests/decisions、knowledge_relations、asset_projections、knowledge_sources；
--       003 documents/document_versions；002 workspaces。016 > 015（015 为
--       knowledge_asset_versions.updated_at），迁移顺序下自然满足。
-- DDL、约束、索引、不变量严格按 design-docs/16 §2 实现，不自行增删字段或改约束语义。

-- Wiki Space：受同一 Schema 与维护策略约束的页面集合（16 §2.1）
CREATE TABLE wiki_spaces (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                VARCHAR(500) NOT NULL,
    -- Schema Document：描述页面类别、命名、引用和维护规则（12 §4.2）
    schema_asset_id     UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE RESTRICT,
    schema_version_id   UUID NOT NULL REFERENCES knowledge_asset_versions(id) ON DELETE RESTRICT,
    -- 确定性维护 index/log Document Asset（12 §4.2，也属 document 资产）
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
    UNIQUE (workspace_id, name)
);
CREATE INDEX idx_wiki_spaces_workspace ON wiki_spaces(workspace_id) WHERE status = 'active';

-- Wiki 页面维护视图（16 §2.2，12 §4.2 wiki_pages）
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

-- 页面来源依赖：生成某页版本时实际读取的来源版本集合（16 §2.3，12 §4.2 wiki_page_sources）
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

-- Maintenance Run：一次维护调度的不可变快照（16 §2.4，12 §4.2 wiki_maintenance_runs）
CREATE TABLE wiki_maintenance_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wiki_space_id       UUID NOT NULL REFERENCES wiki_spaces(id) ON DELETE CASCADE,
    trigger_type        VARCHAR(20) NOT NULL,   -- ingest|query_file|lint|manual
    -- 幂等键：相同输入集合 + Schema + 模型 + Prompt 的重试保持幂等（12 §7.5）
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

-- 页面候选修订：维护器产出的 PagePatch 落为此表行（16 §2.4，12 §7.5）
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
