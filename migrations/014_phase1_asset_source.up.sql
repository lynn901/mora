-- 014_phase1_asset_source.up.sql
-- Phase 1 控制面表：来源/投影/治理/关系（design-docs/14 §2.1，决策 D9）。
-- 在 Phase 0 的 013 八表之上补齐：knowledge_sources / source_sync_runs /
-- knowledge_source_targets / knowledge_relations / review_requests /
-- review_decisions / asset_projections 七表；回补 013 的
-- knowledge_asset_versions.source_id 外键；补 legacy_migration 系统治理 Profile 行。
--
-- 依赖：013_knowledge_core（knowledge_assets/versions、governance_profiles、
--       knowledge_jobs、outbox_events）；003 documents/document_versions（native 引用）。
-- DDL、约束、索引、不变量严格按 design-docs/14 §2 实现，不自行增删字段或改约束语义。

-- 来源（14 §2.1 knowledge_sources）
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

-- 同步 Run（14 §2.1 source_sync_runs）
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

-- 来源 Target → Asset 映射（14 §2.1 knowledge_source_targets）
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

-- 资产关系（14 §2.1 knowledge_relations）
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

-- 审核请求（14 §2.1 review_requests）
-- 每次批准/拒绝/合并/提升/废弃写不可变 decision（14 §4.2 治理）。
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

-- 审核决策（不可变；资产当前状态只是这些决策的投影，14 §2.1）
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

-- 资产投影（14 §2.1 asset_projections，14 §4.5）
-- 任何投影都必须记录 asset_version_id/projection_kind/provider/provider_version/
-- build_revision/built_at，以支持重建、对账和问题定位（§4.1）。
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

-- 回补 013 的 source_id FK（013 建列时无 source 表，未加 FK，14 §2.1）
ALTER TABLE knowledge_asset_versions
  ADD CONSTRAINT fk_versions_source
  FOREIGN KEY (source_id) REFERENCES knowledge_sources(id) ON DELETE SET NULL;

-- legacy_migration 系统治理 Profile（14 §2.2）。
-- governance_profiles.workspace_id NOT NULL，故每个 workspace 各补一行系统 Profile；
-- 存量文档 backfill 走此 Profile（系统服务账号批准，直接 published，不进团队审核收件箱）。
-- 新 workspace 的 Profile 由 backfill/注册路径 upsert，不依赖本迁移；此处只覆盖迁移时
-- 已存在的 workspace（§2 原则：迁移只建结构 + 补系统行，不写业务数据）。
-- review_roles=[] 不进审核收件箱；required_projections=[fts,vector]；auto_publish 标记
-- legacy_migration 来源可由系统直接批准发布。
INSERT INTO governance_profiles
    (workspace_id, name, asset_type, transition_rules, review_roles, auto_publish,
     required_projections, is_system)
SELECT
    w.id,
    'legacy_migration',
    'document',
    '{}'::jsonb,
    '[]'::jsonb,
    '{"legacy_migration": true}'::jsonb,
    '["fts","vector"]'::jsonb,
    true
FROM workspaces w
WHERE NOT EXISTS (
    SELECT 1 FROM governance_profiles g
    WHERE g.workspace_id = w.id AND g.name = 'legacy_migration'
);
