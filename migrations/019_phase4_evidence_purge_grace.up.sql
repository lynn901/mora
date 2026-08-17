-- 019_phase4_evidence_purge_grace.up.sql
-- Phase 4 删除传播宽限起点（design-docs/18 §9.2 / §2.4 D3）。
-- §9.2: active → pending_purge（先停止展开，原文仍可审计）→ purge_after 到期 →
-- purged（擦除 encrypted_content / storage_key）。purge_after 是
-- memory_retention_policies.purge_after 间隔，宽限期需要一个**起点**——即
-- 进入 pending_purge 的时刻——才能算出「宽限到期」是否到来。
--
-- 018 建表时未留 pending_purged_at：expires_at 只能表达「保留到期」点，无法
-- 表达「进入待擦除态」点；显式删除（无 expires_at）更无起点。本迁移补一列
-- pending_purged_at TIMESTAMPTZ：
--   - MarkPendingPurge 时置 now()；
--   - Purge 时置 purged_at（不变）；
--   - 对账 reaper：state='pending_purge' AND pending_purged_at + purge_after ≤ now
--     的行执行擦除。
--
-- 原则（对齐 013/014/018）：只补结构，不写业务数据。该列 NULL 表示 018 既存
-- 行未经过待擦除态（仍是 active），reaper 不作用于它们。回滚反序 DROP 列。
--
-- 幂等：ADD COLUMN IF NOT EXISTS，重复执行无副作用（schema_migrations 已防重）。

ALTER TABLE memory_evidence
  ADD COLUMN IF NOT EXISTS pending_purged_at TIMESTAMPTZ;

-- pending_purge 到期扫描索引：state='pending_purge' 的行按 pending_purged_at
-- 排序，reaper LIMIT 批量取到期项（对齐 018 idx_evidence_purge_due 模式）。
CREATE INDEX IF NOT EXISTS idx_evidence_purge_ready
  ON memory_evidence(pending_purged_at)
  WHERE state = 'pending_purge' AND pending_purged_at IS NOT NULL;
