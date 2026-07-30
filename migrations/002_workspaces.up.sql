-- migrations/002_workspaces.up.sql
-- 工作区与目录（对应 03-data-model.md §2.2）

CREATE TABLE workspaces (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    slug        VARCHAR(255) NOT NULL UNIQUE,
    description TEXT,
    owner_id    UUID NOT NULL REFERENCES users(id),
    settings    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 目录（无限极树，ltree 物化路径）
CREATE TABLE directories (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    parent_id     UUID REFERENCES directories(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    path          LTREE NOT NULL,
    sort_order    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 父目录必须属于同一工作区（CHECK 约束不支持子查询，用触发器实现）
CREATE OR REPLACE FUNCTION chk_directory_parent_workspace() RETURNS trigger AS $$
BEGIN
    IF NEW.parent_id IS NOT NULL THEN
        IF NOT EXISTS (SELECT 1 FROM directories p
                       WHERE p.id = NEW.parent_id AND p.workspace_id = NEW.workspace_id) THEN
            RAISE EXCEPTION 'parent directory must belong to the same workspace';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_directory_parent_workspace
    BEFORE INSERT OR UPDATE ON directories
    FOR EACH ROW EXECUTE FUNCTION chk_directory_parent_workspace();

CREATE INDEX idx_directories_workspace ON directories(workspace_id);
CREATE INDEX idx_directories_parent ON directories(parent_id);
CREATE INDEX idx_directories_path ON directories USING GIST(path);
CREATE INDEX idx_directories_workspace_path ON directories(workspace_id, path);
