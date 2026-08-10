-- 013_knowledge_core.down.sql
-- knowledge_assets 与 knowledge_asset_versions 互引（current_version_id 自引用 FK 为
-- DEFERRABLE INITIALLY DEFERRED）。用 CASCADE 删除，自动解除自引用 FK，顺序无关。
DROP TABLE IF EXISTS knowledge_jobs;
DROP TABLE IF EXISTS outbox_deliveries;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS authorization_decisions;
DROP TABLE IF EXISTS delegated_sessions;
DROP TABLE IF EXISTS agent_bindings;
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS knowledge_asset_versions CASCADE;
DROP TABLE IF EXISTS knowledge_assets CASCADE;
DROP TABLE IF EXISTS governance_profiles;
DROP TABLE IF EXISTS workspace_authz_revisions;
