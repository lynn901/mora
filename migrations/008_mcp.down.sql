-- migrations/008_mcp.down.sql
-- 回滚 MCP 域（设计文档 03 §2.8）。注意：审计日志 audit_logs 不在此处删除，
-- 由其自身的迁移脚本管理（mcp_tool_calls.audit_log_id 仅引用，不级联）。

DROP TABLE IF EXISTS mcp_tool_calls;
DROP TABLE IF EXISTS mcp_sessions;
DROP TABLE IF EXISTS api_tokens;
