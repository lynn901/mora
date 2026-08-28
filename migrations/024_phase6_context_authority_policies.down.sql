-- 024_phase6_context_authority_policies.down.sql
-- Phase 6 Context Broker 回滚（design-docs/19 §2.4）。
-- 先 DROP context_eval_runs 再 DROP context_authority_policies（§2.4 原序）。
-- 两表均无被其他表引用的外键，删除顺序安全；IF EXISTS 保证幂等回滚。

DROP TABLE IF EXISTS context_eval_runs;
DROP TABLE IF EXISTS context_authority_policies;
