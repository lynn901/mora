-- migrations/005_rbac.up.sql
-- RBAC 权限（对应 03-data-model.md §2.5）
-- 决策优先级：显式拒绝 > 显式允许 > 继承 > 默认拒绝

CREATE TABLE roles (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(100) NOT NULL,
    scope         VARCHAR(20) NOT NULL,
    workspace_id  UUID REFERENCES workspaces(id) ON DELETE CASCADE,
    permissions   JSONB NOT NULL DEFAULT '[]',
    is_system     BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE permissions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_type  VARCHAR(20) NOT NULL,
    subject_id    UUID NOT NULL,
    role_id       UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    target_type   VARCHAR(20) NOT NULL,
    target_id     UUID NOT NULL,
    effect        VARCHAR(10) NOT NULL DEFAULT 'allow',
    inherit_scope VARCHAR(20) NOT NULL DEFAULT 'subtree',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    UUID REFERENCES users(id)
);

CREATE INDEX idx_permissions_subject ON permissions(subject_type, subject_id);
CREATE INDEX idx_permissions_target ON permissions(target_type, target_id);
CREATE INDEX idx_permissions_role ON permissions(role_id);
CREATE INDEX idx_permissions_target_inherit ON permissions(target_type, target_id, inherit_scope);

-- 系统内置角色
INSERT INTO roles (name, scope, permissions, is_system) VALUES
    ('super_admin', 'system',  '["read","write","admin"]'::jsonb, true),
    ('workspace_admin', 'workspace', '["read","write","admin"]'::jsonb, true),
    ('editor', 'directory', '["read","write"]'::jsonb, true),
    ('viewer', 'directory', '["read"]'::jsonb, true)
ON CONFLICT DO NOTHING;
