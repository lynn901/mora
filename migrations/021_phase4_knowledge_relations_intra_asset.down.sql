-- 021_phase4_knowledge_relations_intra_asset.down.sql
-- 回滚 Phase 4 去重/冲突 intra-asset 边支持（design-docs/18 §6.1 §8.3 D7；架构裁定路径 A）。
-- 反序：先 DROP 索引，再 DROP 列，再恢复原 CHECK。幂等（IF EXISTS）。
--
-- 注意：恢复原 CHECK 后，已写入的同资产 contradicts/supersedes 边（from_asset_id
-- = to_asset_id）会违反约束——回滚前需先清理这些行（由调用方负责，迁移只
-- 还原结构，不删业务数据，对齐 013/014 原则）。

DROP INDEX IF EXISTS idx_relations_from_unit;
ALTER TABLE knowledge_relations DROP COLUMN IF EXISTS to_unit_id;
ALTER TABLE knowledge_relations DROP COLUMN IF EXISTS from_unit_id;

ALTER TABLE knowledge_relations DROP CONSTRAINT IF EXISTS knowledge_relations_check;
ALTER TABLE knowledge_relations
  ADD CONSTRAINT knowledge_relations_check
  CHECK (from_asset_id <> to_asset_id);
