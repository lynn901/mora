-- 019_phase4_evidence_purge_grace.down.sql
-- 回滚 Phase 4 删除传播宽限起点（design-docs/18 §9.2 / §2.4 D3）。
-- 反序：先 DROP 索引，再 DROP 列。幂等（IF EXISTS）。

DROP INDEX IF EXISTS idx_evidence_purge_ready;
ALTER TABLE memory_evidence DROP COLUMN IF EXISTS pending_purged_at;
