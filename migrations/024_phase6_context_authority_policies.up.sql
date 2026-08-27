-- 024_phase6_context_authority_policies.up.sql
-- Phase 6 Context Broker 数据层地基（design-docs/19 §2.1 / §2.2）。
--
-- 本迁移只建控制面表（策略配置 + 评测记录），不新增内容表：Broker 是编排层，
-- 所有内容仍由各类型引擎的权威记录与投影承载（§2 开篇）。
--   1. context_authority_policies —— 版本化权威策略配置（workspace+intent 维度），
--      policy_version 递增，is_current 排他约束 + WHERE 索引（§2.1）。
--   2. context_eval_runs —— 离线评测运行记录（dataset_tag + 逐 case report_json，§2.2）。
--
-- schema 与约束逐字对齐 §2.1 / §2.2 的 DDL（含 CHECK / EXCLUDE / 索引）。
-- 幂等：CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS。

-- 1. context_authority_policies（§2.1）
--    每个 workspace + intent 有一份策略，policy_version 递增，is_current 排他。
--    config JSONB 的结构由架构提供 schema，首版四内置策略默认值由 PM 治理。
CREATE TABLE IF NOT EXISTS context_authority_policies (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    intent       TEXT NOT NULL,                          -- spec|revision|rationale|procedure (§9.5)
    policy_version INT NOT NULL,                          -- 递增，审计引用
    is_current   BOOLEAN NOT NULL DEFAULT TRUE,           -- 同 (workspace_id,intent) 仅一行 true
    -- 策略内容：各 asset_type 的 authority 权重、首要依据、必须展示的冲突
    config       JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_at TIMESTAMPTZ,                           -- 被新版本取代时置位
    created_by_id UUID,                                  -- 审核/配置者
    CONSTRAINT chk_authority_intent CHECK (intent IN ('spec','revision','rationale','procedure')),
    CONSTRAINT chk_authority_one_current EXCLUDE (workspace_id WITH =, intent WITH =) WHERE (is_current)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_authority_policies_current
    ON context_authority_policies (workspace_id, intent) WHERE is_current;
CREATE INDEX IF NOT EXISTS idx_authority_policies_version
    ON context_authority_policies (workspace_id, intent, policy_version DESC);

-- 2. context_eval_runs（§2.2）
--    评测 runner 的运行记录（D11）。case 集本身是 Go 测试代码（同 codegraph/eval
--    先例），此表记录每次跑批的聚合指标，供发布前阈值锁定比对。
CREATE TABLE IF NOT EXISTS context_eval_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    dataset_tag  TEXT NOT NULL,                          -- 评测集版本标签
    intent       TEXT,                                   -- nil = 全意图
    asset_type   TEXT,                                   -- nil = 全类型（Recall@K/nDCG 按类型分报）
    recall_at_k  DOUBLE PRECISION,
    ndcg         DOUBLE PRECISION,
    citation_accuracy DOUBLE PRECISION,                 -- 引用正确率
    p95_latency_ms INT,
    case_count   INT NOT NULL,
    pass         BOOLEAN NOT NULL,                      -- 是否达到锁定阈值
    report_json  JSONB NOT NULL,                        -- 完整逐 case 结果
    CONSTRAINT chk_eval_intent CHECK (intent IS NULL OR intent IN ('spec','revision','rationale','procedure'))
);

CREATE INDEX IF NOT EXISTS idx_eval_runs_dataset ON context_eval_runs (dataset_tag, run_at DESC);
