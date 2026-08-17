-- 018_phase4_agent_memory.up.sql
-- Phase 4：Agent 记忆沉淀基座（design-docs/18 §2 表结构、§4 Evidence 存储/脱敏/ACL、
-- §10 迁移；决策 D1–D4）。
-- 新增六表：memory_evidence / memory_units / memory_evidence_links /
--          memory_retention_policies / memory_feedback / memory_dedup_suggestions。
-- Memory 不新增 asset_type，memory_units 以 asset_id 引用既有
-- knowledge_assets(asset_type='memory')（013 已预留该枚举）。Evidence ACL 复用 Phase 0
-- permissions.target_type='evidence'（domain.TargetEvidence，rbac.go:60），不新增 ACL 表。
--
-- 依赖：013 knowledge_assets/knowledge_asset_versions/workspaces/workspace_authz_revisions；
--       014 knowledge_relations(supersedes/contradicts)/review_requests/review_decisions/
--       asset_projections；005 permissions（target_type='evidence' 已预留）。
--       018 > 016（016 为 Phase 2 Wiki 维护），迁移顺序下自然满足。
-- 原则：只建表 + 约束 + 索引 + 补 memory_retention_policies 系统默认行（对齐 013/014
-- §2 原则，不写业务数据）。DDL、约束、索引、不变量严格按 design-docs/18 §2 实现，
-- 不自行增删字段或改约束语义。

-- 保留策略：先建，供 memory_evidence.retention_policy_id 外键引用（§2.4）。
CREATE TABLE memory_retention_policies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    memory_type   VARCHAR(20),                       -- NULL=该 workspace 所有类型默认
    retain_for    INTERVAL NOT NULL,                 -- 保留期限；到期先 pending_purge
    purge_after   INTERVAL,                          -- pending_purge 后宽限至硬擦除（审计 hash 保留）
    is_system     BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, memory_type)
);

-- 原始证据 L0，独立 ACL（D2）。来源分五类（12 §4.4 source_kind）。
-- source_asset_id 不加 FK —— 来源资产删除走 §7.2 删除传播（标 evidence_missing），
-- 不级联删 Evidence。小片段（≤64KiB 脱敏后）AES-256-GCM 加密存
-- encrypted_content（D4）；大对象存 MinIO mora-evidence/<ws>/<id>，DB 只存 storage_key +
-- content_hash + redacted_excerpt。purged 后清空 encrypted_content/storage_key，只留
-- content_hash + redacted_excerpt + 审计元数据（12 §8.4）。
CREATE TABLE memory_evidence (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    owner_type        VARCHAR(20) NOT NULL,          -- user|group|agent|service_account
    owner_id          UUID NOT NULL,
    source_kind       VARCHAR(20) NOT NULL,          -- session|message|tool_call|document|code
    source_ref        TEXT NOT NULL,                 -- 不可执行定位（session_id / message_id / tool_call_id / asset_version_id 引用）
    source_asset_id         UUID,                    -- 引用 knowledge_assets(id)，不加 FK：来源删除走删除传播而非级联
    source_asset_version_id UUID,                    -- 引用 knowledge_asset_versions(id)，不加 FK：同上
    visibility        VARCHAR(20) NOT NULL DEFAULT 'private',  -- private|restricted
    captured_authz_revision BIGINT NOT NULL,         -- 入库时 workspace_authz_revisions.revision，仅供审计，不作今后授权
    content_hash      TEXT NOT NULL,                 -- 脱敏后内容的 SHA-256，用于去重与删除证明
    encrypted_content BYTEA,                         -- 小片段：AES-256-GCM 密文（D4）；NULL 则用 storage_key
    storage_key       TEXT,                          -- 大对象：MinIO key mora-evidence/<ws>/<id>（D4）
    key_version       INTEGER,                       -- envelope KEK 版本（D4 密钥轮换）；encrypted_content 非空时必填
    redacted_excerpt  TEXT NOT NULL,                 -- 最小脱敏片段，无权读原文时返回此列
    classification    VARCHAR(40),                   -- 自动敏感分类标签（secret|credential|pii|none）
    retention_policy_id UUID REFERENCES memory_retention_policies(id),
    state             VARCHAR(20) NOT NULL DEFAULT 'active',  -- active|pending_purge|purged
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ,                   -- 保留策略到期点；到期先置 pending_purge
    purged_at         TIMESTAMPTZ,                   -- 内容擦除时间；purged 后只保留 id/hash/审计元数据
    deleted_at        TIMESTAMPTZ,
    CHECK (source_kind IN ('session','message','tool_call','document','code')),
    CHECK (visibility IN ('private','restricted')),
    CHECK (state IN ('active','pending_purge','purged')),
    CHECK ((encrypted_content IS NOT NULL AND storage_key IS NULL AND key_version IS NOT NULL)
        OR (encrypted_content IS NULL     AND storage_key IS NOT NULL))
);
CREATE INDEX idx_evidence_workspace_owner ON memory_evidence(workspace_id, owner_type, owner_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_evidence_source_asset ON memory_evidence(source_asset_id) WHERE source_asset_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX idx_evidence_purge_due ON memory_evidence(expires_at) WHERE state = 'active' AND expires_at IS NOT NULL;

-- 提炼后的结构化记忆单元，以 asset_id 挂到 knowledge_assets(asset_type='memory')（D1）。
-- state='published' 必须经 review_decision（§6.2），首版无自动发布（附录 A 不变量 9）。
-- evidence_missing=true 的单元不作为高权威依据召回（12 §8.4）。superseded_by 仅在
-- reviewer 确认 merge/supersede 后由 governance 写入。
CREATE TABLE memory_units (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    asset_id          UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    asset_version_id  UUID REFERENCES knowledge_asset_versions(id) ON DELETE SET NULL,
    memory_type       VARCHAR(20) NOT NULL,           -- fact|decision|constraint|preference|event
    statement         TEXT NOT NULL,                  -- 自然语言结论（脱敏后）
    structured_payload JSONB NOT NULL DEFAULT '{}',   -- 实体键/有效期/scope（受 JSON Schema 约束）
    confidence        NUMERIC(5,4),
    valid_from        TIMESTAMPTZ,
    expires_at        TIMESTAMPTZ,
    state             VARCHAR(20) NOT NULL DEFAULT 'candidate',  -- candidate|approved|published|rejected|deprecated
    superseded_by     UUID,                            -- merge/supersede 候选建议指向；发布前由 reviewer 确认
    evidence_missing  BOOLEAN NOT NULL DEFAULT false,  -- Evidence 删除/不可定位后置 true（D3）
    authority         NUMERIC(5,4) NOT NULL DEFAULT 0.5,  -- 召回排序用，反馈 + 治理状态影响
    created_by_type   VARCHAR(20) NOT NULL,
    created_by_id     UUID NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (memory_type IN ('fact','decision','constraint','preference','event')),
    CHECK (state IN ('candidate','approved','published','rejected','deprecated')),
    CHECK (NOT (state = 'published' AND superseded_by IS NOT NULL))
);
CREATE INDEX idx_units_workspace_state ON memory_units(workspace_id, state) WHERE state IN ('candidate','published');
CREATE INDEX idx_units_asset ON memory_units(asset_id, created_at DESC);
CREATE INDEX idx_units_type_time ON memory_units(workspace_id, memory_type, valid_from DESC);
CREATE INDEX idx_units_supersede ON memory_units(superseded_by) WHERE superseded_by IS NOT NULL;
CREATE INDEX idx_units_evidence_missing ON memory_units(workspace_id) WHERE evidence_missing = true;
-- 结构化键精确召回（12 §8.5）——实体键索引，受 structured_payload schema 约束
CREATE INDEX idx_units_structured ON memory_units USING gin (structured_payload);

-- Memory ↔ Evidence 多对多链接，记录引用定位与支撑/冲突类型（12 §4.4）。
CREATE TABLE memory_evidence_links (
    memory_unit_id UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    evidence_id    UUID NOT NULL REFERENCES memory_evidence(id) ON DELETE CASCADE,
    quote_locator  JSONB,                            -- 不可执行引用定位（offset/range/hash），不含原文
    support_type   VARCHAR(20) NOT NULL DEFAULT 'supports',  -- supports|contradicts
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_unit_id, evidence_id),
    CHECK (support_type IN ('supports','contradicts'))
);
CREATE INDEX idx_evidence_links_evidence ON memory_evidence_links(evidence_id);

-- useful/incorrect/stale 反馈（D8）。反馈不改事实正文，只影响 authority/freshness 与
-- revalidate 触发。
CREATE TABLE memory_feedback (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_unit_id  UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    feedback_type   VARCHAR(20) NOT NULL,            -- useful|incorrect|stale
    given_by_type   VARCHAR(20) NOT NULL,
    given_by_id     UUID NOT NULL,
    rationale_redacted TEXT,                         -- 脱敏理由
    revalidate_triggered BOOLEAN NOT NULL DEFAULT false,  -- stale/incorrect 是否触发 revalidate Job
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (feedback_type IN ('useful','incorrect','stale'))
);
CREATE INDEX idx_feedback_unit ON memory_feedback(memory_unit_id, created_at DESC);

-- 去重/冲突建议（不自动合并，D7）。reviewer 处置后落 memory_units.superseded_by 或
-- knowledge_relations。
CREATE TABLE memory_dedup_suggestions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    unit_a_id       UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    unit_b_id       UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    suggestion_type VARCHAR(20) NOT NULL,            -- duplicate|extends|contradicts|unrelated
    origin          VARCHAR(20) NOT NULL DEFAULT 'generated',  -- rule|generated
    confidence      NUMERIC(5,4),
    evidence_ref    JSONB,                            -- 建议依据（召回分数/规则命中），不含原文
    state           VARCHAR(20) NOT NULL DEFAULT 'pending',  -- pending|accepted|rejected
    resolved_by_type VARCHAR(20),
    resolved_by_id  UUID,
    resolved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (suggestion_type IN ('duplicate','extends','contradicts','unrelated')),
    CHECK (origin IN ('rule','generated')),
    CHECK (state IN ('pending','accepted','rejected')),
    CHECK (unit_a_id <> unit_b_id)
);
CREATE INDEX idx_dedup_pending ON memory_dedup_suggestions(workspace_id, state) WHERE state = 'pending';

-- 系统默认保留策略：为每个已存在 workspace 补一条 NULL memory_type 默认行
-- （§2.4；§19.6 具体期限值由 PM 治理填入，此处用可配置默认值 365 天保留 +
-- 30 天 purge 宽限）。幂等：WHERE NOT EXISTS 防重。
INSERT INTO memory_retention_policies (workspace_id, memory_type, retain_for, purge_after, is_system)
SELECT id, NULL, INTERVAL '365 days', INTERVAL '30 days', true
FROM workspaces w
WHERE NOT EXISTS (SELECT 1 FROM memory_retention_policies p WHERE p.workspace_id = w.id AND p.memory_type IS NULL);
