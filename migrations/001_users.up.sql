-- migrations/001_users.up.sql
-- 用户与身份（对应 03-data-model.md §2.1）

CREATE EXTENSION IF NOT EXISTS "pgcrypto";
-- ltree 目录树物化路径
CREATE EXTENSION IF NOT EXISTS "ltree";

-- zhparser 中文分词：若系统未安装则降级为 simple（应用层通过 FTS_CONFIG 切换）。
-- 不能用 CREATE EXTENSION IF NOT EXISTS（扩展不可用时仍会报错），用 DO 块捕获。
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS "zhparser";
EXCEPTION WHEN OTHERS THEN
    -- zhparser 未安装时跳过；全文检索降级为 simple 配置
    NULL;
END $$;

-- zhparser 中文全文检索配置（若扩展存在）
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'zhparser') THEN
        CREATE TEXT SEARCH CONFIGURATION chinese_zh (PARSER = zhparser);
        ALTER TEXT SEARCH CONFIGURATION chinese_zh
            ADD MAPPING FOR n,v,a,i,e,l WITH simple;
    END IF;
EXCEPTION WHEN OTHERS THEN
    NULL;
END $$;

-- 用户
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         VARCHAR(255) NOT NULL UNIQUE,
    name          VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255),
    avatar_url    TEXT,
    status        VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 用户组（对接组织架构）
CREATE TABLE groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

-- 服务账号（供 API Token 绑定）
CREATE TABLE service_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 系统内置超级管理员（占位，实际由部署脚本注入）
INSERT INTO users (email, name, status) VALUES
    ('admin@wiki.local', 'System Admin', 'active')
ON CONFLICT (email) DO NOTHING;
