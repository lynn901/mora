-- 024_phase6_context_authority_policies.up.sql
-- Phase 6-S1 (YS-202 §1) 数据层：权威策略配置表 + 离线评测记录表。
-- design-docs/19-phase6-context-broker.md §2.1 / §2.2。
--
-- 设计约束（不可破坏）：
--   - Phase 6 不新增内容表（§2「Broker 是编排层，所有内容仍由各类型引擎的
--     权威记录与投影承载」）。仅新增控制面配置/评测表，可重建，非权威内容。
--   - context_authority_policies 版本化：同 (workspace_id,intent) 仅一行
--     is_current=true（EXCLUDE 排他约束 + WHERE 唯一索引，双保险），
--     policy_version 递增，审计引用。
--   - context_eval_runs 独立，无外键（评测产物，可重建）。
--   - 幂等：CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS
--     （schema_migrations 已防重，重复执行无副作用）。
--   - 全部参数化（07-security §10）：本迁移不含用户输入，DDL 固定。

-- EXCLUDE 排他约束对 UUID/TEXT 列需要 btree_gist（001 已建 pgcrypto/ltree
-- 同 pattern）。幂等：CREATE EXTENSION IF NOT EXISTS。
CREATE EXTENSION IF NOT EXISTS "btree_gist";

-- §2.1 版本化权威策略配置表。
CREATE TABLE IF NOT EXISTS context_authority_policies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    intent          TEXT NOT NULL,                          -- spec|revision|rationale|procedure (§9.5)
    policy_version   INT NOT NULL,                          -- 递增，审计引用
    is_current       BOOLEAN NOT NULL DEFAULT TRUE,         -- 同 (workspace_id,intent) 仅一行 true
    -- 策略内容：各 asset_type 的 authority 权重、首要依据、必须展示的冲突
    config          JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_at   TIMESTAMPTZ,                           -- 被新版本取代时置位
    created_by_id   UUID,                                  -- 审核/配置者
    CONSTRAINT chk_authority_intent CHECK (intent IN ('spec','revision','rationale','procedure')),
    -- 同 (workspace_id,intent) 至多一行 is_current=true。需要 btree_gist；
    -- 若不可用，下方唯一索引 idx_authority_policies_current 兜底（语义等价）。
    CONSTRAINT chk_authority_one_current EXCLUDE (workspace_id WITH =, intent WITH =) WHERE (is_current)
);

-- is_current 排他：唯一索引兜底（与 EXCLUDE 约束语义等价，且不依赖 btree_gist）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_authority_policies_current
    ON context_authority_policies (workspace_id, intent) WHERE is_current;

-- 按版本递减检索历史（加载当前策略 / 审计回溯）。
CREATE INDEX IF NOT EXISTS idx_authority_policies_version
    ON context_authority_policies (workspace_id, intent, policy_version DESC);

-- §2.2 离线评测运行记录表。
CREATE TABLE IF NOT EXISTS context_eval_runs (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    dataset_tag        TEXT NOT NULL,                          -- 评测集版本标签
    intent             TEXT,                                   -- nil = 全意图
    asset_type         TEXT,                                   -- nil = 全类型（Recall@K/nDCG 按类型分报）
    recall_at_k        DOUBLE PRECISION,
    ndcg               DOUBLE PRECISION,
    citation_accuracy  DOUBLE PRECISION,                      -- 引用正确率
    p95_latency_ms     INT,
    case_count         INT NOT NULL,
    pass               BOOLEAN NOT NULL,                       -- 是否达到锁定阈值
    report_json        JSONB NOT NULL,                         -- 完整逐 case 结果
    CONSTRAINT chk_eval_intent CHECK (intent IS NULL OR intent IN ('spec','revision','rationale','procedure'))
);

CREATE INDEX IF NOT EXISTS idx_eval_runs_dataset
    ON context_eval_runs (dataset_tag, run_at DESC);
