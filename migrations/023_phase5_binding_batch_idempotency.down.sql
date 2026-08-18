-- 023_phase5_binding_batch_idempotency.down.sql
-- 回滚 Phase 5-2 批量幂等表（design-docs/19 §5.2 / §11.1）。
-- 反序：先 DROP 索引再 DROP 表。agent_bindings 结构未改，无需还原。
-- 幂等（IF EXISTS）。对齐 013/014/021/022 回滚原则：只还原结构。

DROP INDEX IF EXISTS idx_binding_batches_agent;
DROP TABLE IF EXISTS agent_binding_batches;
