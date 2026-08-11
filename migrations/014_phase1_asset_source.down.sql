-- 014_phase1_asset_source.down.sql
-- 反序回滚 Phase 1 控制面表（design-docs/14 §2.1 末段）。
-- 先 DROP source_id 外键，再按依赖反序 DROP 七表，最后清理 legacy_migration 系统治理 Profile 行。

-- 先回补的 FK：knowledge_asset_versions.source_id
ALTER TABLE knowledge_asset_versions
  DROP CONSTRAINT IF EXISTS fk_versions_source;

-- 反序 DROP：依赖最深（被引用最多）的最后建、最先删。
-- asset_projections → review_decisions → review_requests → knowledge_relations
-- → knowledge_source_targets → source_sync_runs → knowledge_sources
DROP TABLE IF EXISTS asset_projections;
DROP TABLE IF EXISTS review_decisions;
DROP TABLE IF EXISTS review_requests;
DROP TABLE IF EXISTS knowledge_relations;
DROP TABLE IF EXISTS knowledge_source_targets;
DROP TABLE IF EXISTS source_sync_runs;
DROP TABLE IF EXISTS knowledge_sources;

-- 清理本迁移补的 legacy_migration 系统治理 Profile 行（14 §2.2）。
-- 只删系统行，不动其它 Profile。
DELETE FROM governance_profiles
WHERE is_system = true AND name = 'legacy_migration';
