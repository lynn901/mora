-- 024_phase6_context_authority_policies.down.sql
-- 回滚 Phase 6-S1（YS-202 §1）。design-docs/19-phase6-context-broker.md §2.4。
-- 顺序：先 DROP context_eval_runs（无依赖），再 DROP context_authority_policies。
-- 幂等：DROP TABLE IF EXISTS（schema_migrations 已防重）。
DROP TABLE IF EXISTS context_eval_runs;
DROP TABLE IF EXISTS context_authority_policies;
