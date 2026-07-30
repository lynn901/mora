-- migrations/010_seed_demo.down.sql
-- 回滚联调种子数据（不删除管理员账户本身，仅清空其密码与种子数据）。

DELETE FROM api_tokens WHERE token_hash = '15611f355a858b9800b308d515aaaba205a0859283ad42af86578894da069a07';

DELETE FROM permissions
WHERE target_type = 'workspace' AND target_id = '11111111-1111-1111-1111-111111111111'
  AND subject_type = 'user'
  AND subject_id = (SELECT id FROM users WHERE email = 'admin@wiki.local');

DELETE FROM embedding_models WHERE provider = 'tei' AND model_name = 'sentence-transformers/all-MiniLM-L6-v2';

DELETE FROM workspaces WHERE id = '11111111-1111-1111-1111-111111111111';

UPDATE users SET password_hash = NULL WHERE email = 'admin@wiki.local';
