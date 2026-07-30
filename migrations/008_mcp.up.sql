-- migrations/008_mcp.up.sql
-- MCP 域：API Token、MCP 会话、MCP 工具调用记录（设计文档 03 §2.8, 06 §7）
-- 依赖：001 用户与身份（users / service_accounts）、006 审计日志（audit_logs）

-- API Token（MCP 鉴权凭证，明文不落库，仅存 SHA-256 哈希）
CREATE TABLE IF NOT EXISTS api_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(255) NOT NULL,
    token_hash    VARCHAR(255) NOT NULL UNIQUE,     -- 存哈希，不存明文
    prefix        VARCHAR(20)  NOT NULL,             -- 明文前缀（展示用，如 wki_xxxx）
    identity_type VARCHAR(20)  NOT NULL,             -- user/service_account
    identity_id   UUID NOT NULL,                    -- users.id 或 service_accounts.id
    scope         VARCHAR(20)  NOT NULL DEFAULT 'readonly',  -- readonly/readwrite/admin
    expires_at    TIMESTAMPTZ,                      -- NULL = 永不过期
    revoked_at    TIMESTAMPTZ,                      -- NULL = 未吊销（即时吊销置非空）
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    UUID REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_tokens_identity ON api_tokens(identity_type, identity_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tokens_hash     ON api_tokens(token_hash) WHERE revoked_at IS NULL;

-- MCP 会话（initialize 时创建，DELETE /mcp 或超时结束）
CREATE TABLE IF NOT EXISTS mcp_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id      UUID NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    transport     VARCHAR(20)  NOT NULL,             -- http_sse/stdio
    client_info   JSONB,                            -- Agent 名称/版本
    capabilities  JSONB,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_mcp_sessions_token ON mcp_sessions(token_id, started_at DESC);

-- MCP 工具/资源调用记录（追加写，仅 INSERT；关联不可篡改审计日志）
CREATE TABLE IF NOT EXISTS mcp_tool_calls (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id     UUID NOT NULL REFERENCES mcp_sessions(id) ON DELETE CASCADE,
    tool_name      VARCHAR(100) NOT NULL,
    params_summary JSONB NOT NULL DEFAULT '{}',      -- 参数摘要（脱敏）
    result_status  VARCHAR(20)  NOT NULL,             -- success/forbidden/error
    target_resource TEXT,                             -- 操作目标资源标识
    duration_ms    INTEGER,
    audit_log_id   UUID,                              -- 关联 audit_logs.id
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mcp_calls_session ON mcp_tool_calls(session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_mcp_calls_tool    ON mcp_tool_calls(tool_name, created_at DESC);
