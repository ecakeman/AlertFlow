CREATE SCHEMA IF NOT EXISTS gateway;

CREATE TABLE IF NOT EXISTS gateway.agent_principals (
    agent_id VARCHAR(128) PRIMARY KEY,
    credential_hash CHAR(64) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS gateway.agent_tool_permissions (
    agent_id VARCHAR(128) NOT NULL REFERENCES gateway.agent_principals(agent_id),
    provider_id VARCHAR(128) NOT NULL,
    tool_name VARCHAR(200) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (agent_id, provider_id, tool_name)
);

CREATE TABLE IF NOT EXISTS gateway.agent_invocations (
    caller_agent_id VARCHAR(128) NOT NULL,
    invocation_id VARCHAR(128) NOT NULL,
    target_agent_id VARCHAR(128) NOT NULL,
    protocol VARCHAR(16) NOT NULL,
    skill_id VARCHAR(128) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    remote_message_id VARCHAR(128),
    status VARCHAR(32) NOT NULL,
    result_json TEXT,
    error_code VARCHAR(64),
    safe_message TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (caller_agent_id, invocation_id)
);
