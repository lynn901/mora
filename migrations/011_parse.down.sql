-- migrations/011_parse.down.sql
-- 回滚文档解析域迁移（10 §4.2.2）

DROP TABLE IF EXISTS chunk_relations;
DROP TABLE IF EXISTS parse_configs;
DROP TABLE IF EXISTS parse_tasks;

ALTER TABLE documents
    DROP COLUMN IF EXISTS parse_task_id,
    DROP COLUMN IF EXISTS parse_status,
    DROP COLUMN IF EXISTS source_format,
    DROP COLUMN IF EXISTS storage_key;

DROP INDEX IF EXISTS idx_documents_storage_key;
DROP INDEX IF EXISTS idx_documents_parse_status;
