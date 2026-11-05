package com.alertflow.integration.mcp;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.HexFormat;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;

@Component
public class McpPolicy {
    private final Map<String, String> tokenToAgent = new ConcurrentHashMap<>();
    private final Set<String> grants = ConcurrentHashMap.newKeySet();
    private final JdbcTemplate jdbc;

    public McpPolicy(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
        remember(System.getenv("ALERTFLOW_MAIN_MCP_TOKEN"), "alertflow-main-agent");
        remember(System.getenv("KNOWLEDGE_AGENT_MCP_TOKEN"), "knowledge-agent");
    }

    public static McpPolicy memory() {
        return new McpPolicy(null);
    }

    public void remember(String token, String agent) {
        if (token == null || token.isBlank()) {
            return;
        }
        tokenToAgent.put(token, agent);
    }

    public void grant(String agent, String provider, String tool) {
        grants.add(agent + "\n" + provider + "\n" + tool);
    }

    public String agentFor(String authorization) {
        if (authorization == null || !authorization.startsWith("Bearer ")) {
            throw new IllegalStateException("missing bearer token");
        }
        String token = authorization.substring("Bearer ".length());
        String agent = tokenToAgent.get(token);
        if (agent != null) {
            return agent;
        }
        if (jdbc == null) {
            throw new IllegalStateException("unknown principal");
        }
        try {
            String hash = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(token.getBytes(StandardCharsets.UTF_8)));
            return jdbc.query("SELECT agent_id FROM agent_principals WHERE credential_hash=? AND enabled",
                    (rs, n) -> rs.getString(1), hash).stream().findFirst()
                    .orElseThrow(() -> new IllegalStateException("unknown principal"));
        } catch (IllegalStateException error) {
            throw error;
        } catch (Exception error) {
            throw new IllegalStateException("unknown principal");
        }
    }

    public boolean allowed(String agent, String provider, String tool) {
        if (grants.contains(agent + "\n" + provider + "\n" + tool)) {
            return true;
        }
        if (jdbc == null) {
            return false;
        }
        try {
            Integer count = jdbc.queryForObject("""
                    SELECT count(*) FROM agent_tool_permissions
                    WHERE agent_id=? AND provider_id=? AND tool_name=? AND enabled
                    """, Integer.class, agent, provider, tool);
            return count != null && count > 0;
        } catch (Exception error) {
            return false;
        }
    }
}
