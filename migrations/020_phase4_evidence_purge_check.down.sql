-- 020_phase4_evidence_purge_check.down.sql
-- 反向回滚：删除 020 的命名收窄约束，恢复 018 原始的无条件存储分裂 CHECK。
-- 回滚到 018 行为即等价于「撤回本迁移」，故不需要重建列或其它结构动作。
-- 注意：恢复后 Purge 将再次触发 CHECK 违反——这是 018 的既有缺陷状态，
-- down 仅用于迁移可逆性，不应在生产回滚（见 020 up 注释）。

-- 1. 删除所有引用 encrypted_content 的 CHECK 约束（幂等）：
--    既删 020 的命名约束，也清掉之前 up/down 循环可能残留的旧匿名约束，
--    否则重复 up→down→up→down 会让匿名 CHECK 累积。锚点选 encrypted_content
--    （018 四条 CHECK 里只有存储分裂一条引用它），与 up 的 DO 块一致。
DO $$
DECLARE
    c RECORD;
BEGIN
    FOR c IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'memory_evidence'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%encrypted_content%'
    LOOP
        EXECUTE format('ALTER TABLE memory_evidence DROP CONSTRAINT IF EXISTS %I', c.conname);
    END LOOP;
END $$;

-- 2. 恢复 018 原始匿名 CHECK（无条件二选一）。
ALTER TABLE memory_evidence
  ADD CHECK (
    (encrypted_content IS NOT NULL AND storage_key IS NULL AND key_version IS NOT NULL)
    OR (encrypted_content IS NULL     AND storage_key IS NOT NULL)
  );
