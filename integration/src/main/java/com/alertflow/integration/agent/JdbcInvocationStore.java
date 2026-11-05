package com.alertflow.integration.agent;

import java.util.Optional;

import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;

@Component
public class JdbcInvocationStore implements InvocationStore {
    private final JdbcTemplate jdbc;

    public JdbcInvocationStore(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    @Override
    public Optional<Row> find(String caller, String invocationId) {
        return jdbc.query("""
                SELECT caller_agent_id, invocation_id, target_agent_id, skill_id, request_hash,
                       status, result_json, error_code, safe_message
                FROM agent_invocations WHERE caller_agent_id=? AND invocation_id=?
                """, (rs, n) -> new Row(rs.getString(1), rs.getString(2), rs.getString(3), rs.getString(4),
                rs.getString(5), rs.getString(6), rs.getString(7), rs.getString(8), rs.getString(9)),
                caller, invocationId).stream().findFirst();
    }

    @Override
    public void insert(Row row) {
        jdbc.update("""
                INSERT INTO agent_invocations
                (caller_agent_id, invocation_id, target_agent_id, protocol, skill_id, request_hash, status, created_at, updated_at)
                VALUES (?,?,?,'A2A',?,?,?,now(),now())
                """, row.caller(), row.invocationId(), row.target(), row.skill(), row.requestHash(), row.status());
    }

    @Override
    public void finish(String caller, String invocationId, String status, String resultJson, String errorCode, String safeMessage) {
        jdbc.update("""
                UPDATE agent_invocations
                SET status=?, result_json=?, error_code=?, safe_message=?, updated_at=now(),
                    finished_at=CASE WHEN ? IN ('COMPLETED','FAILED','TIMED_OUT','UNKNOWN') THEN now() ELSE finished_at END
                WHERE caller_agent_id=? AND invocation_id=?
                """, status, resultJson, errorCode, safeMessage, status, caller, invocationId);
    }
}
