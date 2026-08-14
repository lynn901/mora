-- 015: add knowledge_asset_versions.updated_at.
--
-- The §6/§7 activation path (asset_activation.go) flips a version's
-- build_status (pending→ready) and stamps a ready-only mark when a stale
-- version fails the CAS — both are legitimate, append-only-compatible state
-- transitions on a version row (the row itself is never rewritten; version_no
-- and content are immutable per AC-6). The table lacked an updated_at because
-- 013 treated versions as fully immutable; the async activation path needs a
-- last-modified timestamp for reconcile/observability. Adds the column with a
-- safe default and back-fills existing rows to their created_at.
ALTER TABLE knowledge_asset_versions
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

UPDATE knowledge_asset_versions
SET updated_at = created_at
WHERE updated_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_asset_versions_updated
    ON knowledge_asset_versions(updated_at DESC);
