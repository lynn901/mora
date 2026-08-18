-- 023_phase5_binding_batch_idempotency.up.sql
-- Phase 5-2 (YS-162) 配装批量幂等表。design-docs/19 §5.2 / §11.1。
--
-- 设计约束（不可破坏）：
--   - 不改 agent_bindings 表结构（Phase 0 已建，migration 022 只补索引）。
--   - Idempotency-Key 是 batch 级语义：同 agent 一次批量配装创建/更新多条
--     binding，key 绑定到这次 batch，不是单条 binding。因此 key 不能挂在
--     agent_bindings 上（那是单行 1:1），需要一个 batch 记录表承载。
--
-- 本表只存 batch 元数据 + payload hash 用于幂等校验；binding 正文仍在
-- agent_bindings。同 key 同 payload → 幂等重试（返回原 batch 的 bindings）；
-- 同 key 不同 payload → ErrIdempotencyConflict（§11.1）。payload_hash 是输入
-- 集合的规范化 hash（service 层计算），不含密钥/令牌。
--
-- 原则对齐 013/014/021/022：只补结构，不写业务数据；幂等
-- (CREATE TABLE IF NOT EXISTS)。

CREATE TABLE IF NOT EXISTS agent_binding_batches (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key   TEXT NOT NULL UNIQUE,           -- §11.1 caller-supplied (or generated)
    agent_id          UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    payload_hash      TEXT NOT NULL,                  -- canonical hash of the batch inputs (no secrets)
    payload           JSONB NOT NULL DEFAULT '{}',     -- {binding_ids, items} for idempotent-retry re-fetch
    binding_count     INTEGER NOT NULL DEFAULT 0,     -- number of bindings written in this batch
    authz_revision    BIGINT NOT NULL,                -- workspace_authz_revisions.revision after the batch (§5.4)
    actor_type        VARCHAR(20),                    -- who triggered the batch (audit)
    actor_id          UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 按 agent 列出 batch（管理面 GET /agents/{id}/bindings:batch 历史查询用）。
CREATE INDEX IF NOT EXISTS idx_binding_batches_agent
    ON agent_binding_batches(agent_id, created_at DESC)
    WHERE idempotency_key IS NOT NULL;
