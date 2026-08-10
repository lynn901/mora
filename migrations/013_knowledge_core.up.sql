-- 013_knowledge_core.up.sql
-- Phase 0 控制面核心表（13 §2.1–2.2、§4.2–4.3、§5.6、§6.3、§6.5）
-- 原则：只建表 + 约束 + 索引，不写业务数据（仅补 workspace_authz_revisions 元数据行）。
-- 依赖：001 users/service_accounts、002 workspaces、003 documents/document_versions、
--       005 rbac、008 mcp(api_tokens)。013 > 008，迁移顺序下自然满足。

-- 工作区授权 revision（撤权线性化点，13 §5.6）
-- 每个 workspace 恒有一行；revision 单调递增，由同一事务负责 +1。
CREATE TABLE workspace_authz_revisions (
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    revision     BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id)
);

-- 治理 Profile（13 §4.2）
CREATE TABLE governance_profiles (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id         UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                 VARCHAR(200) NOT NULL,
    asset_type           VARCHAR(20),              -- document|codebase|memory|skill|NULL=通用
    transition_rules     JSONB NOT NULL DEFAULT '{}',
    review_roles         JSONB NOT NULL DEFAULT '[]',
    auto_publish         JSONB NOT NULL DEFAULT '{}',
    default_validity     INTERVAL,
    evidence_required    BOOLEAN NOT NULL DEFAULT false,
    required_projections JSONB NOT NULL DEFAULT '[]', -- [fts|vector|summary|codegraph|relation]
    is_system            BOOLEAN NOT NULL DEFAULT false,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);
CREATE INDEX idx_gov_profiles_workspace ON governance_profiles(workspace_id);

-- 知识资产版本（13 §4.2）——先建版本表，资产表的 current_version_id 自引用 FK 在其后追加。
CREATE TABLE knowledge_asset_versions (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id                  UUID NOT NULL,                  -- 自引用 FK，资产表建完后追加
    version_no                BIGINT NOT NULL,
    source_id                 UUID,                  -- Phase 1 才有 source 表，先建列不加 FK
    source_revision           TEXT,
    native_document_version_id UUID REFERENCES document_versions(id),
    content_origin            VARCHAR(20) NOT NULL DEFAULT 'human', -- human|imported|generated|system
    generation_ref            JSONB,
    provider_ref              JSONB,
    content_hash              TEXT,
    dedupe_key                TEXT NOT NULL,
    build_status              VARCHAR(20) NOT NULL DEFAULT 'pending',
    governance_status        VARCHAR(20) NOT NULL DEFAULT 'candidate',
    activation_policy_snapshot JSONB,
    approved_by_type          VARCHAR(20),
    approved_by_id            UUID,
    approved_at               TIMESTAMPTZ,
    created_by_type           VARCHAR(20) NOT NULL,
    created_by_id             UUID NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (asset_id, version_no),
    UNIQUE (asset_id, dedupe_key)
);
-- 注：部分唯一约束（含 WHERE）PostgreSQL 仅支持 CREATE UNIQUE INDEX，不内联于表定义。
CREATE UNIQUE INDEX uq_versions_native_doc_version
    ON knowledge_asset_versions(native_document_version_id)
    WHERE native_document_version_id IS NOT NULL;
CREATE INDEX idx_versions_asset ON knowledge_asset_versions(asset_id, version_no DESC);
CREATE INDEX idx_versions_build ON knowledge_asset_versions(build_status) WHERE build_status IN ('pending','building');
CREATE INDEX idx_versions_governance ON knowledge_asset_versions(governance_status);

-- 知识资产（13 §4.2）
CREATE TABLE knowledge_assets (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id             UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    asset_type              VARCHAR(20) NOT NULL,   -- document|codebase|memory|skill
    name                    VARCHAR(500) NOT NULL,
    description             TEXT,
    owner_type              VARCHAR(20) NOT NULL,   -- user|group|agent|service_account
    owner_id                UUID NOT NULL,
    status                  VARCHAR(20) NOT NULL DEFAULT 'draft',
    visibility              VARCHAR(20) NOT NULL DEFAULT 'private',
    governance_profile_id   UUID REFERENCES governance_profiles(id),
    native_document_id      UUID REFERENCES documents(id),  -- 仅 document 类型非空
    current_version_id      UUID,                  -- 自引用，下方追加 FK
    latest_requested_version_no BIGINT NOT NULL DEFAULT 0,
    confidence              NUMERIC(5,4),
    valid_from              TIMESTAMPTZ,
    expires_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_assets_workspace ON knowledge_assets(workspace_id, asset_type);
CREATE INDEX idx_assets_owner ON knowledge_assets(owner_type, owner_id);
CREATE INDEX idx_assets_status ON knowledge_assets(status) WHERE status NOT IN ('archived','rejected');
-- 部分唯一约束：document 类型资产与原生文档一一对应（仅 native_document_id 非空时）。
CREATE UNIQUE INDEX uq_assets_native_doc
    ON knowledge_assets(asset_type, native_document_id)
    WHERE native_document_id IS NOT NULL;

-- 资产表先于版本表创建，故版本表 asset_id 的自引用 FK 在此追加。
ALTER TABLE knowledge_asset_versions
  ADD CONSTRAINT fk_versions_asset
  FOREIGN KEY (asset_id) REFERENCES knowledge_assets(id) ON DELETE CASCADE;
-- current_version_id 自引用 FK（DEFERRABLE INITIALLY DEFERRED）：
-- 版本激活 CAS 在同一事务内先写 versions 再写 assets.current_version_id。
ALTER TABLE knowledge_assets
  ADD CONSTRAINT fk_assets_current_version
  FOREIGN KEY (current_version_id) REFERENCES knowledge_asset_versions(id) DEFERRABLE INITIALLY DEFERRED;

-- Agent（13 §4.3）
CREATE TABLE agents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name              VARCHAR(255) NOT NULL,
    description       TEXT,
    owner_id          UUID NOT NULL REFERENCES users(id),
    status            VARCHAR(20) NOT NULL DEFAULT 'active',  -- active|suspended|revoked
    runtime_type      TEXT,
    service_account_id UUID REFERENCES service_accounts(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_agents_workspace ON agents(workspace_id) WHERE status = 'active';
CREATE INDEX idx_agents_owner ON agents(owner_id);

-- Agent Binding（13 §4.3）
CREATE TABLE agent_bindings (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id          UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    scope_kind        VARCHAR(20) NOT NULL,   -- asset|workspace|asset_type
    asset_id          UUID REFERENCES knowledge_assets(id),
    asset_type        VARCHAR(20),
    effect            VARCHAR(10) NOT NULL DEFAULT 'allow',  -- allow|deny
    version_policy    VARCHAR(20) NOT NULL DEFAULT 'follow_published', -- follow_published|pinned
    pinned_version_id UUID REFERENCES knowledge_asset_versions(id),
    delivery_mode     VARCHAR(20) NOT NULL DEFAULT 'tool',   -- tool|summary|inline
    priority          INTEGER NOT NULL DEFAULT 0,
    created_by        UUID REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at        TIMESTAMPTZ,
    CHECK (
        (scope_kind = 'asset' AND asset_id IS NOT NULL)
        OR (scope_kind = 'asset_type' AND asset_type IS NOT NULL)
        OR (scope_kind = 'workspace')
    ),
    CHECK (NOT (version_policy = 'pinned' AND pinned_version_id IS NULL)),
    CHECK (NOT (version_policy = 'pinned' AND scope_kind <> 'asset'))
);
CREATE INDEX idx_bindings_agent ON agent_bindings(agent_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_bindings_workspace ON agent_bindings(workspace_id) WHERE revoked_at IS NULL;

-- 委托会话（13 §5.1/§5.6）——服务端可撤销记录，客户端只持 JTI
CREATE TABLE delegated_sessions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id              UUID REFERENCES api_tokens(id) ON DELETE CASCADE,
    agent_id              UUID REFERENCES agents(id) ON DELETE CASCADE,
    acting_user_id       UUID REFERENCES users(id),
    workspace_id          UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    allowed_actions       JSONB NOT NULL DEFAULT '[]',   -- 允许动作集合
    issued_authz_revision BIGINT NOT NULL,
    expires_at            TIMESTAMPTZ NOT NULL,
    revoked_at            TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_delegated_sessions_token ON delegated_sessions(token_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_delegated_sessions_agent ON delegated_sessions(agent_id) WHERE revoked_at IS NULL;

-- 授权决策记录（13 §5.6，审计与 Provider capability 校验用）
CREATE TABLE authorization_decisions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    authz_revision  BIGINT NOT NULL,
    principal_type  VARCHAR(20) NOT NULL,
    principal_id    UUID NOT NULL,
    acting_user_id  UUID REFERENCES users(id),
    agent_id        UUID REFERENCES agents(id),
    action          VARCHAR(20) NOT NULL,
    scope_hash      TEXT NOT NULL,      -- 规范化授权范围的 hash，防篡改
    audience        TEXT,               -- 目标 Provider/内部服务
    nonce_hash      TEXT,               -- 单次 nonce 的 hash
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_authz_decisions_workspace ON authorization_decisions(workspace_id, authz_revision);
CREATE INDEX idx_authz_decisions_lookup ON authorization_decisions(workspace_id, principal_type, principal_id) WHERE revoked_at IS NULL;

-- 事务 Outbox（13 §6.3）
CREATE TABLE outbox_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  VARCHAR(40) NOT NULL,
    aggregate_id    UUID NOT NULL,
    event_type      VARCHAR(80) NOT NULL,
    event_version   INT NOT NULL DEFAULT 1,
    workspace_id    UUID,
    actor_type      VARCHAR(20),
    actor_id        UUID,
    destinations    TEXT[] NOT NULL DEFAULT '{}',
    payload         JSONB NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempt         INT NOT NULL DEFAULT 0,
    last_error      TEXT
);
CREATE INDEX idx_outbox_unpublished ON outbox_events(occurred_at) WHERE published_at IS NULL;

CREATE TABLE outbox_deliveries (
    outbox_event_id   UUID NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    stream            TEXT NOT NULL,
    delivery_attempt  INT NOT NULL DEFAULT 1,
    delivered_at      TIMESTAMPTZ,
    last_error        TEXT,
    PRIMARY KEY (outbox_event_id, stream)
);

-- 知识任务（13 §6.5，Phase 0 只建表与基础库）
CREATE TABLE knowledge_jobs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_event_id   UUID,                  -- 触发本 Job 的 outbox_events.id（不加 FK，允许跨保留期）
    job_type          VARCHAR(60) NOT NULL,
    asset_id          UUID REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    asset_version_id UUID REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    source_id         UUID,
    target_key        TEXT,
    build_revision    TEXT,
    dedupe_key        TEXT NOT NULL UNIQUE,
    status            VARCHAR(20) NOT NULL DEFAULT 'pending',
    attempt           INT NOT NULL DEFAULT 0,
    max_attempt       INT NOT NULL DEFAULT 5,
    lease_owner       TEXT,
    lease_until       TIMESTAMPTZ,
    progress          JSONB,
    error_code        VARCHAR(60),
    error_detail_redacted TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_jobs_status ON knowledge_jobs(status, lease_until) WHERE status IN ('pending','running');

-- §2.2 元数据补齐：给现有 workspaces 写入 revision=0 行，撤权线性化首次 +1 不撞无行。
-- 这是元数据补齐，不是业务 backfill（§16.1 明确不做生产业务数据 backfill）。
INSERT INTO workspace_authz_revisions(workspace_id, revision)
SELECT id, 0 FROM workspaces
ON CONFLICT (workspace_id) DO NOTHING;
