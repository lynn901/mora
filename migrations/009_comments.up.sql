-- migrations/009_comments.up.sql
-- 评论（对应 03-data-model.md §2.9）

CREATE TABLE comments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    block_id      UUID,
    parent_id     UUID REFERENCES comments(id) ON DELETE CASCADE,
    author_id     UUID NOT NULL REFERENCES users(id),
    content       TEXT NOT NULL,
    mentions      UUID[],
    resolved      BOOLEAN NOT NULL DEFAULT false,
    resolved_by   UUID REFERENCES users(id),
    resolved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_comments_document ON comments(document_id, created_at DESC);
CREATE INDEX idx_comments_block ON comments(document_id, block_id) WHERE block_id IS NOT NULL;
CREATE INDEX idx_comments_parent ON comments(parent_id);
CREATE INDEX idx_comments_unresolved ON comments(document_id) WHERE resolved = false;
