-- migrations/004_tags.up.sql
-- 标签（对应 03-data-model.md §2.4）

CREATE TABLE tags (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          VARCHAR(100) NOT NULL,
    color         VARCHAR(20),
    parent_id     UUID REFERENCES tags(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, name, parent_id)
);

CREATE INDEX idx_tags_workspace ON tags(workspace_id);

CREATE TABLE document_tags (
    document_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    tag_id      UUID NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, tag_id)
);

CREATE INDEX idx_doctags_tag ON document_tags(tag_id);
