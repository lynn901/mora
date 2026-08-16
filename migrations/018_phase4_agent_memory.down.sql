-- 018_phase4_agent_memory.down.sql
-- 回滚 Phase 4 Agent 记忆沉淀六表（design-docs/18 §2.8）。
-- 按反向依赖顺序 DROP：memory_dedup_suggestions → memory_feedback →
-- memory_evidence_links → memory_units → memory_evidence →
-- memory_retention_policies。
-- 回滚不删 knowledge_assets(asset_type='memory') 行（属业务数据，迁移只建结构，
-- 对齐 013/014 §2 原则）。memory_evidence.source_asset_id / source_asset_version_id
-- 无 FK，DROP 不受其约束；memory_units.asset_id / asset_version_id 的 CASCADE / SET
-- NULL 由 DROP 顺序自然解除。

DROP TABLE IF EXISTS memory_dedup_suggestions;
DROP TABLE IF EXISTS memory_feedback;
DROP TABLE IF EXISTS memory_evidence_links;
DROP TABLE IF EXISTS memory_units;
DROP TABLE IF EXISTS memory_evidence;
DROP TABLE IF EXISTS memory_retention_policies;
