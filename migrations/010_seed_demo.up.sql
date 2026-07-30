-- migrations/010_seed_demo.up.sql
-- 联调种子数据：让 docker compose up 后即可跑通完整链路（登录→建文档→RAG 索引→MCP 检索）。
-- 仅用于本地/联调演示，生产部署不应执行此迁移。全部幂等（ON CONFLICT / WHERE NOT EXISTS）。
-- 依赖：001 users、002 workspaces、005 rbac(roles/permissions)、007 rag(embedding_models)、008 mcp(api_tokens)。

-- 1. 管理员密码：admin@wiki.local / admin123 —— 登录获取 JWT，创建文档触发 doc_event。
--    明文不落库，仅存 bcrypt 哈希（与 handler.HashPassword/CheckPassword 一致）。
UPDATE users SET password_hash = '$2a$10$dC.Hu.NG4Ar/pGyqlgbKRuV2DL.vOElANeE5EYsuipJkIkXiEKWoS'
WHERE email = 'admin@wiki.local' AND (password_hash IS NULL OR password_hash = '');

-- 2. 演示工作区（固定 UUID，便于权限/文档引用）。owner = 管理员。
INSERT INTO workspaces (id, name, slug, description, owner_id)
SELECT '11111111-1111-1111-1111-111111111111', '工程知识库', 'eng-wiki', '联调演示工作区', u.id
FROM users u WHERE u.email = 'admin@wiki.local'
ON CONFLICT (id) DO NOTHING;

-- 3. 授予管理员对该工作区的读权限（viewer 角色，workspace subtree 继承）。
--    使 RAG ViewerScope / ResolveReaders 解析出 user:<admin> 主体，检索可见。
INSERT INTO permissions (subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope)
SELECT 'user', u.id, r.id, 'workspace', '11111111-1111-1111-1111-111111111111', 'allow', 'subtree'
FROM users u, roles r
WHERE u.email = 'admin@wiki.local' AND r.name = 'viewer' AND r.is_system = true
  AND NOT EXISTS (
    SELECT 1 FROM permissions p
    WHERE p.subject_type='user' AND p.subject_id=u.id
      AND p.target_type='workspace' AND p.target_id='11111111-1111-1111-1111-111111111111'
  )
LIMIT 1;

-- 4. 激活 Embedding 模型（TEI / Qwen3-Embedding-0.6B / dim 1024）—— rag-worker 索引与检索必需。
--    与 config 默认 EMBEDDING_PROVIDER=tei / EMBEDDING_MODEL=Qwen/Qwen3-Embedding-0.6B / EMBEDDING_DIM=1024 对齐。
INSERT INTO embedding_models (provider, model_name, dimension, max_token, status)
VALUES ('tei', 'Qwen/Qwen3-Embedding-0.6B', 1024, 8192, 'active')
ON CONFLICT (provider, model_name) DO UPDATE SET
    dimension = EXCLUDED.dimension, max_token = EXCLUDED.max_token,
    status = 'active', updated_at = now();

-- 5. MCP API Token（绑定管理员用户，readwrite）—— mcp-server prod 模式鉴权。
--    明文：wki_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4 （与 mock 模式 dev token 一致，便于联调复用）。
--    仅存 SHA-256 哈希（auth.HashToken）。
INSERT INTO api_tokens (name, token_hash, prefix, identity_type, identity_id, scope)
SELECT 'dev-token', '15611f355a858b9800b308d515aaaba205a0859283ad42af86578894da069a07',
       'wki_dev_a1b2', 'user', u.id, 'readwrite'
FROM users u WHERE u.email = 'admin@wiki.local'
ON CONFLICT (token_hash) DO NOTHING;
