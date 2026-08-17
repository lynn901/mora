-- 020_phase4_evidence_purge_check.up.sql
-- 修复 memory_evidence 存储分裂 CHECK 与 purged 态的冲突（design-docs/18 §8.4 / §2.1 D4）。
--
-- 背景：018 建表时的表级 CHECK 要求 encrypted_content / storage_key 二选一
-- （inline 密文 OR MinIO 大对象 key），原意是 D4「小片段加密存 PG、大对象存
-- MinIO，二者不并存」。但该约束写成了无条件 XOR：
--   CHECK ((encrypted_content IS NOT NULL AND storage_key IS NULL AND key_version IS NOT NULL)
--       OR (encrypted_content IS NULL     AND storage_key IS NOT NULL))
--
-- 缺陷：Purge（infra/postgres/memory_evidence.go:147）把三列
--   encrypted_content = NULL, storage_key = NULL, key_version = NULL
-- 同时置空——purged 行既无密文也无对象 key，两个分支都不满足，触发 CHECK
-- 违反，Purge 在真实 DB 上必然失败。单元测试用 fake repo 不强制该约束，集成
-- 路径此前无对应测试，故缺陷一直未暴露。
--
-- 设计意图（§8.4「purged 后只保留 id/hash/审计元数据」+ 018 列注释「purged 后
-- 只保留 id/hash/审计元数据」）：purged 行合法地三列全 NULL，存储分裂的
-- 二选一只对**可读态** active / pending_purge 成立。本迁移将 CHECK 收窄到
-- 非 purged 态：
--   - state IN ('active','pending_purge') → 必须二选一（保持 D4 不变量）；
--   - state = 'purged' → 三列全 NULL 合法（擦除证明可验证，hash/excerpt 保留）。
--
-- 原则（对齐 013/014/018/019）：只动结构，不写业务数据。命名约束
-- memory_evidence_storage_split_check 取代 018 的匿名 CHECK，便于后续追溯。
--
-- 幂等：DROP/ADD 均以 IF EXISTS / IF NOT EXISTS 保护，重复执行无副作用
-- （schema_migrations 已防重）。

-- 1. 删除 018 的存储分裂 CHECK。
--    018 的表级 CHECK 是匿名的，PG 按自身规则命名（PG16 落到
--    memory_evidence_check，其它版本/顺序可能落到 check{,1,2,3}），名字不
--    稳定，故不按名删，而按定义内容定位。关键坑：PG 存储约束定义时会**重
--    排括号**——018 写的 `encrypted_content IS NOT NULL AND storage_key IS
--    NULL` 被存成 `(encrypted_content IS NOT NULL) AND (storage_key IS NULL)`，
--    形如 `A AND B` 的连续子串被 `) AND (` 打断，故不能按带 AND 的短语匹配。
--    改用最稳的锚点：约束定义中出现裸 `encrypted_content` 这一标识符——018
--    的四条 CHECK 里只有存储分裂这一条引用 encrypted_content（其余是
--    source_kind / visibility / state），命中唯一。
--    该 DO 块同时会命中本迁移上一轮已建的 memory_evidence_storage_split_check
--    （它也引用 encrypted_content）并先行删除，使后续 ADD 命名约束幂等：
--    重复执行 020 后仍只剩一条命名约束，不会残留旧的匿名约束，也不会因
--    约束名已存在而 ADD 失败（PG 不支持 ADD CONSTRAINT IF NOT EXISTS）。
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

-- 2. 重加收窄版命名约束：存储分裂二选一仅在 active / pending_purge 上强制。
--    （此时旧的匿名 CHECK 已被上面的 DO 块清除，约束名唯一，ADD 必成功。）
ALTER TABLE memory_evidence
  ADD CONSTRAINT memory_evidence_storage_split_check
  CHECK (
    state = 'purged'
    OR (encrypted_content IS NOT NULL AND storage_key IS NULL AND key_version IS NOT NULL)
    OR (encrypted_content IS NULL     AND storage_key IS NOT NULL)
  );
