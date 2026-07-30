-- migrations/003_documents.up.sql
-- 文档、版本、块、附件（对应 03-data-model.md §2.3）

-- 全文检索配置名：zhparser 安装则为 chinese_zh，否则 simple
CREATE TABLE documents (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    directory_id  UUID REFERENCES directories(id) ON DELETE SET NULL,
    title         VARCHAR(500) NOT NULL,
    content       JSONB NOT NULL DEFAULT '[]',
    content_text  TEXT NOT NULL DEFAULT '',
    format        VARCHAR(20) NOT NULL DEFAULT 'blocks',
    status        VARCHAR(20) NOT NULL DEFAULT 'draft',
    index_status  VARCHAR(20) NOT NULL DEFAULT 'pending',
    version_no    INTEGER NOT NULL DEFAULT 1,
    created_by    UUID NOT NULL REFERENCES users(id),
    updated_by    UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 全文检索索引：动态选择配置（chinese_zh 优先，缺失回退 simple）
DO $$
DECLARE
    cfg TEXT;
BEGIN
    SELECT COALESCE(
        (SELECT 'chinese_zh' FROM pg_ts_config WHERE cfgname = 'chinese_zh' LIMIT 1),
        'simple'
    ) INTO cfg;
    EXECUTE format(
        'CREATE INDEX idx_documents_fts ON documents USING GIN(to_tsvector(%L, coalesce(title,'''') || '' '' || coalesce(content_text,''''))) WHERE status != ''deleted''',
        cfg
    );
END $$;

CREATE INDEX idx_documents_workspace ON documents(workspace_id) WHERE status != 'deleted';
CREATE INDEX idx_documents_directory ON documents(directory_id) WHERE status != 'deleted';
CREATE INDEX idx_documents_status ON documents(status, index_status);
CREATE INDEX idx_documents_updated ON documents(updated_at DESC);
CREATE INDEX idx_documents_created_by ON documents(created_by);
CREATE INDEX idx_documents_content_gin ON documents USING GIN(content jsonb_path_ops);

-- 文档版本
CREATE TABLE document_versions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version_no    INTEGER NOT NULL,
    content       JSONB NOT NULL,
    content_text  TEXT NOT NULL DEFAULT '',
    diff_summary  TEXT,
    author_id     UUID NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, version_no)
);

CREATE INDEX idx_versions_document ON document_versions(document_id, version_no DESC);
CREATE INDEX idx_versions_created ON document_versions(created_at DESC);

-- 块（预留结构化查询；MVP 主要存 JSONB）
CREATE TABLE blocks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    block_type    VARCHAR(50) NOT NULL,
    content       JSONB NOT NULL DEFAULT '{}',
    sort_order    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_blocks_document ON blocks(document_id, sort_order);

-- 附件
CREATE TABLE attachments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    name          VARCHAR(500) NOT NULL,
    mime_type     VARCHAR(255) NOT NULL,
    size_bytes    BIGINT NOT NULL,
    storage_key   TEXT NOT NULL,
    storage_type  VARCHAR(20) NOT NULL DEFAULT 'minio',
    uploaded_by   UUID NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_attachments_document ON attachments(document_id);
