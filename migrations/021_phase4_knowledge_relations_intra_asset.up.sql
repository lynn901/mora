-- 021_phase4_knowledge_relations_intra_asset.up.sql
-- Phase 4 去重/冲突 intra-asset 边支持（design-docs/18 §6.1 §8.3 D7；架构裁定路径 A）。
--
-- 背景：014 knowledge_relations 建模**跨资产**边，CHECK (from_asset_id <>
-- to_asset_id) 防自环。但 memory 去重常是**同资产内**的单元对——同一
-- knowledge_assets(asset_type='memory') 行下多个 memory_units（设计 D1），
-- 同一证据源提炼出的同胞单元互相 duplicate/extends/contradicts 是去重最常
-- 见场景。设计 D7 要求 contradicts/supersedes 落 knowledge_relations，但该
-- CHECK 使同资产场景必失败（SQLSTATE 23514），contradicts 边落不进，§8.2
-- 召回"同时返回冲突双方"失去关系边支撑。
--
-- 架构裁定（路径 A）：放宽 CHECK 至
--   (from_asset_id <> to_asset_id OR relation_type IN ('contradicts','supersedes'))
-- ——保留跨资产防自环（其他关系类型仍禁 from=to），放行 memory 的合法 intra-
-- asset contradicts/supersedes。同时一次性补 from_unit_id / to_unit_id 可空
-- 列表达 per-unit 粒度，避免把迁移成本转嫁到 §8.2 召回的每查询 JOIN（line
-- 538/615「召回携带 Relations、不静默选一个答案」要求边的 unit 粒度直接可读）。
--
-- from_unit_id / to_unit_id 不加 FK：memory_units 删除走 §9.2 删除传播级联，
-- knowledge_relations 是 014 既有表（ON DELETE CASCADE on from_asset_id/
-- to_asset_id 已覆盖资产级；unit 级由删除传播在应用层清理 relation 行，不
-- 加 FK 以免与 014 既有级联策略冲突）。跨资产边（既有 derived_from/explains
-- 等）的 *_unit_id 留空，语义不变。
--
-- 幂等：ADD COLUMN IF NOT EXISTS、CREATE INDEX IF NOT EXISTS；CHECK 用 DROP +
-- ADD（constraint 名 PG 自动取 knowledge_relations_check）。回滚反序。
-- 原则对齐 013/014/018/019：只补结构，不写业务数据。

-- 1. 放宽自环 CHECK：跨资产关系仍禁 from=to；contradicts/supersedes 允许同
--    资产（intra-asset memory 边）。
ALTER TABLE knowledge_relations DROP CONSTRAINT IF EXISTS knowledge_relations_check;
ALTER TABLE knowledge_relations
  ADD CONSTRAINT knowledge_relations_check
  CHECK (from_asset_id <> to_asset_id OR relation_type IN ('contradicts','supersedes'));

-- 2. per-unit 粒度列（可空）。contradicts/supersede 边填；跨资产边留空。
ALTER TABLE knowledge_relations ADD COLUMN IF NOT EXISTS from_unit_id UUID;
ALTER TABLE knowledge_relations ADD COLUMN IF NOT EXISTS to_unit_id UUID;

-- 3. per-unit 边召回索引：from_unit_id 非空的行（memory intra-asset 边）按
--    from_unit_id 检索，对齐 014 idx_relations_from 的 partial-index 模式。
CREATE INDEX IF NOT EXISTS idx_relations_from_unit
  ON knowledge_relations(from_unit_id)
  WHERE from_unit_id IS NOT NULL;
