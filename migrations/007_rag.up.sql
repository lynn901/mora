-- migrations/007_rag.up.sql
-- RAG 域：Embedding 模型配置、索引任务、Chunk 元数据
-- 依据：03-data-model.md §2.7 / PRD §6.2 RAG 向量域

-- Embedding 模型配置（TEI / Ollama，兼容 Qwen3-Embedding）
CREATE TABLE IF NOT EXISTS embedding_models (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider          VARCHAR(50) NOT NULL,                 -- tei/ollama
    model_name        VARCHAR(255) NOT NULL,
    dimension         INTEGER NOT NULL,                     -- 1024 等
    max_token         INTEGER NOT NULL DEFAULT 8192,
    instruction_query TEXT,                                 -- 检索 query 前缀（Instruction-Aware）
    instruction_doc   TEXT,                                 -- 入库 doc 前缀
    status            VARCHAR(20) NOT NULL DEFAULT 'active', -- active/inactive
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider, model_name)
);

-- 索引任务（流水线状态机）
CREATE TABLE IF NOT EXISTS indexing_tasks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    event_id      UUID NOT NULL,                            -- 幂等键（事件 ID）
    event_type    VARCHAR(32) NOT NULL,                     -- create/update/delete/permission_change/model_rebuild
    status        VARCHAR(20) NOT NULL DEFAULT 'pending',   -- pending/processing/indexed/failed
    attempt       INTEGER NOT NULL DEFAULT 0,
    max_attempt   INTEGER NOT NULL DEFAULT 3,
    payload       JSONB NOT NULL DEFAULT '{}',              -- 事件详情
    error_message TEXT,
    model_id      UUID REFERENCES embedding_models(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_indexing_tasks_status ON indexing_tasks(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_indexing_tasks_document ON indexing_tasks(document_id);
CREATE INDEX IF NOT EXISTS idx_indexing_tasks_pending ON indexing_tasks(status, created_at) WHERE status IN ('pending','failed');
CREATE INDEX IF NOT EXISTS idx_indexing_tasks_event ON indexing_tasks(event_id);

-- Chunk 元数据（向量本体存 Qdrant，此表记录元数据用于对账/补偿/级联清理）
CREATE TABLE IF NOT EXISTS chunks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id     UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version_no      INTEGER NOT NULL,
    chunk_index     INTEGER NOT NULL,
    text            TEXT NOT NULL,
    token_count     INTEGER,
    section_path    TEXT,
    model_id        UUID NOT NULL REFERENCES embedding_models(id),
    qdrant_point_id UUID NOT NULL,                          -- Qdrant 中对应 point ID（uuid5 确定性）
    metadata        JSONB NOT NULL DEFAULT '{}',            -- {workspace_id,directory_id,tags,visible_to}
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, version_no, chunk_index)
);

CREATE INDEX IF NOT EXISTS idx_chunks_document ON chunks(document_id, version_no);
CREATE INDEX IF NOT EXISTS idx_chunks_qdrant ON chunks(qdrant_point_id);
CREATE INDEX IF NOT EXISTS idx_chunks_model ON chunks(model_id);

-- 补偿扫描：未投递事件 / 处理中过久任务由 rag-worker 扫描重投
-- documents.index_status 列已在 003_documents.up.sql 定义（pending/processing/indexed/failed）
