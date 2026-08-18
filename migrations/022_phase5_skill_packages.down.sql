-- 022_phase5_skill_packages.down.sql
-- 回滚 Phase 5 数据层地基（design-docs/19 §3.1 / §3.2 / §3.3）。
-- 反序：先 DROP agent_bindings 补充索引（不改表结构，列不变），再 DROP
-- skill_packages 表。幂等（IF EXISTS）。对齐 013/014/021 回滚原则：只还原结构。

DROP INDEX IF EXISTS idx_bindings_agent_effect_priority;
DROP INDEX IF EXISTS idx_bindings_agent_scope;

DROP INDEX IF EXISTS idx_skill_packages_format;
DROP INDEX IF EXISTS idx_skill_packages_validation;

DROP TABLE IF EXISTS skill_packages;
