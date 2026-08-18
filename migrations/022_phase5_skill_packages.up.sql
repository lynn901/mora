-- 022_phase5_skill_packages.up.sql
-- Phase 5 数据层地基（design-docs/19 §3.1 / §3.2 / §3.3；字段对齐 design-docs/12 §4.5）。
--
-- 本迁移只建数据层结构，不含 service/handler/MCP 实现：
--   1. skill_packages 表：Skill 包作为受治理资产版本挂载在 knowledge_asset_versions
--      上（1:1），存 manifest / original_frontmatter / content_hash /
--      validation_report / compatibility_report / scanner_version 等。
--   2. agent_bindings 补充索引（不改表结构）：支撑 Phase 5 生效集解析——
--      idx_bindings_agent_scope 一次查询解析某 Agent 全部生效 Binding；
--      idx_bindings_agent_effect_priority 支撑 effect/priority 决策代数排序。
--
-- 原则对齐 013/014/018/019/021：只补结构 + 索引，不写业务数据。
-- 幂等：CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS；表已存在
-- 时仅补缺失索引（schema_migrations 已防重）。

-- 1. skill_packages（§3.1）
--    asset_version_id 既是 PK 又是 FK → 与 knowledge_asset_versions 1:1。
--    ON DELETE CASCADE：版本删除连带清包，资产下多版本不互拷正文（Phase 5 §9 门禁）。
--    validation_status 默认 pending：导入即 pending，校验后置 passed/failed/opaque；
--      passed 仅表“可保存交付”≠可执行（Mora 不执行 Skill，§4）。
CREATE TABLE IF NOT EXISTS skill_packages (
    asset_version_id      UUID PRIMARY KEY,                       -- 1:1 挂载版本
    storage_key           TEXT NOT NULL,                          -- MinIO 不可变归档原件定位（不可执行选择器）
    format_id             VARCHAR(60) NOT NULL,                   -- 包格式标识（agentskills.io/<spec-version> / hermes/* / opaque）
    schema_version        TEXT NOT NULL,                          -- 规格版本快照
    manifest              JSONB NOT NULL DEFAULT '{}',   -- 规范化清单（文件清单/能力声明摘要）
    original_frontmatter  JSONB,                                  -- 未知合法字段原样无损保留（往返一致性锚之一）
    content_hash          TEXT NOT NULL,                          -- 往返一致性主锚
    signature            JSONB,                                   -- 签名信息（不含 Secret 值）
    provenance_ref        JSONB,                                  -- 来源追溯引用（不含明文凭据）
    validation_status     VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending|passed|failed|opaque
    validation_report     JSONB,                                  -- findings/hashes/signature 结构（§4.3）
    compatibility_report JSONB,                                  -- delivery + runtime_needs + opaque_fields（§4.3）
    scanner_version       TEXT NOT NULL,                          -- 扫描器版本（结果可复现/对账）
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT skill_packages_asset_version_id_fkey
        FOREIGN KEY (asset_version_id)
        REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    CONSTRAINT skill_packages_validation_status_check
        CHECK (validation_status IN ('pending','passed','failed','opaque'))
);

-- §3.1 索引一：按校验状态筛待处理包（partial-index，对齐 013/014 模式）。
CREATE INDEX IF NOT EXISTS idx_skill_packages_validation
    ON skill_packages(validation_status)
    WHERE validation_status IN ('pending','failed');

-- §3.1 索引二：按格式/profile 统计与兼容性检索。
CREATE INDEX IF NOT EXISTS idx_skill_packages_format
    ON skill_packages(format_id, schema_version);

-- 2. agent_bindings 补充索引（§3.2 / §3.3，不改表结构）
--    生效集只在未撤销 Binding 上解析；既有 idx_bindings_agent 已支撑 agent_id
--    单列检索，以下两条补齐“一次查询解析生效集”所需复合序：
--
--    §3.2 idx_bindings_agent_scope：支撑“一次查询解析某 Agent 全部生效
--        Binding”——agent_id + scope 维度聚合，生效集解析入口。
CREATE INDEX IF NOT EXISTS idx_bindings_agent_scope
    ON agent_bindings(agent_id, scope_kind, asset_id, asset_type)
    WHERE revoked_at IS NULL;

--    §3.3 idx_bindings_agent_effect_priority：支撑决策代数排序——
--        effect（deny 优先于 allow）+ priority DESC，生效集内规则定序。
CREATE INDEX IF NOT EXISTS idx_bindings_agent_effect_priority
    ON agent_bindings(agent_id, effect, priority DESC)
    WHERE revoked_at IS NULL;
