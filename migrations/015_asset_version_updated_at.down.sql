-- 015 down: drop knowledge_asset_versions.updated_at.
DROP INDEX IF EXISTS idx_asset_versions_updated;
ALTER TABLE knowledge_asset_versions DROP COLUMN IF EXISTS updated_at;
