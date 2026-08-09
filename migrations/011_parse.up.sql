-- migrations/011_parse.up.sql
-- 文档解析域：解析任务状态机、解析配置模板、parent-child 分块关系
-- 依据：design-docs/10-document-parsing-design.md §4.2.2 / §6 / §7
-- 向后兼容：documents 新增列均有默认值，存量文档视为已解析（parse_status='parsed'）

-- documents 表新增列（ALTER，向后兼容）
ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS storage_key   TEXT,                                  -- 上传原文件对象存储 key
    ADD COLUMN IF NOT EXISTS source_format VARCHAR(20) NOT NULL DEFAULT '',      -- 上传文件格式: txt/md/html/pdf/docx/...
    ADD COLUMN IF NOT EXISTS parse_status  VARCHAR(20) NOT NULL DEFAULT 'parsed', -- pending/parsing/parsed/failed/skipped
    ADD COLUMN IF NOT EXISTS parse_task_id UUID;                                  -- 关联当前 parse_tasks.id

CREATE INDEX IF NOT EXISTS idx_documents_parse_status ON documents(parse_status) WHERE status != 'deleted';
CREATE INDEX IF NOT EXISTS idx_documents_storage_key ON documents(storage_key) WHERE storage_key IS NOT NULL;

-- 解析任务状态机（与 indexing_tasks 对称，05 §3.1 / 10 §4.3）
CREATE TABLE IF NOT EXISTS parse_tasks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    event_id      UUID NOT NULL,                            -- 幂等键
    status        VARCHAR(20) NOT NULL DEFAULT 'pending',   -- pending/parsing/parsed/failed
    attempt       INTEGER NOT NULL DEFAULT 0,
    max_attempt   INTEGER NOT NULL DEFAULT 3,
    parse_opts    JSONB NOT NULL DEFAULT '{}',              -- 本次解析配置覆盖（§7）
    parser_name   VARCHAR(100),                             -- 实际使用的 parser
    progress      JSONB NOT NULL DEFAULT '[]',              -- 分阶段进度时间线（§6.3）
    error_message TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_parse_tasks_status ON parse_tasks(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_parse_tasks_document ON parse_tasks(document_id);
CREATE INDEX IF NOT EXISTS idx_parse_tasks_pending ON parse_tasks(status, created_at) WHERE status IN ('pending','failed');

-- 解析配置模板（工作区级默认 + 全局默认，供上传时回填，§7）
CREATE TABLE IF NOT EXISTS parse_configs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID REFERENCES workspaces(id) ON DELETE CASCADE,  -- NULL=全局默认
    name          VARCHAR(100) NOT NULL,
    config        JSONB NOT NULL DEFAULT '{}',              -- {chunking_strategy, chunk_size, ...}
    is_default    BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, name)
);

CREATE INDEX IF NOT EXISTS idx_parse_configs_workspace ON parse_configs(workspace_id) WHERE workspace_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_parse_configs_global ON parse_configs(workspace_id) WHERE workspace_id IS NULL;

-- parent-child 分块的父子关系（独立表，避免 chunks 表膨胀；P1，§2.3）
CREATE TABLE IF NOT EXISTS chunk_relations (
    child_chunk_id   UUID NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
    parent_chunk_id  UUID NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
    document_id      UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    PRIMARY KEY (child_chunk_id, parent_chunk_id)
);

CREATE INDEX IF NOT EXISTS idx_chunk_relations_parent ON chunk_relations(parent_chunk_id);
CREATE INDEX IF NOT EXISTS idx_chunk_relations_doc ON chunk_relations(document_id);
