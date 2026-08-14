-- 016_phase2_wiki_maintenance.down.sql
-- 回滚 Phase 2 Wiki 维护表（design-docs/16 §2.6）。
-- Wiki 页面正文仍存于 documents/document_versions，回滚只删维护元数据，不删正文。
-- 按反向依赖顺序删除。

DROP TABLE IF EXISTS wiki_page_proposals;
DROP TABLE IF EXISTS wiki_maintenance_runs;
DROP TABLE IF EXISTS wiki_page_sources;
DROP TABLE IF EXISTS wiki_pages;
DROP TABLE IF EXISTS wiki_spaces;
