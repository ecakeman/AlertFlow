package com.alertflow.integration.agent;

import java.util.List;
import java.util.UUID;

import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Transactional;

@Component
public class JdbcInvocationStore implements InvocationStore {
    private final JdbcTemplate jdbc;

    public JdbcInvocationStore(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    @Override
    @Transactional
    public Claim claim(String caller, String invocationId, String target, String skill, String requestHash, int leaseSeconds) {
        String owner = UUID.randomUUID().toString();
        int inserted = jdbc.update("""
                INSERT INTO agent_invocations
                (caller_agent_id, invocation_id, target_agent_id, protocol, skill_id, request_hash, status,
                 lease_owner, lease_until, attempts, fencing_token, created_at, updated_at)
                VALUES (?,?,?,'A2A',?,?,'RUNNING',?, now() + (? * interval '1 second'), 1, 1, now(), now())
                ON CONFLICT (caller_agent_id, invocation_id) DO NOTHING
                """, caller, invocationId, target, skill, requestHash, owner, leaseSeconds);
        if (inserted == 1) {
            return new Claim("RUN", 1, owner, null, null, null);
        }
        List<Claim> existing = jdbc.query("""
                SELECT request_hash, status, result_json, error_code, safe_message,
                       lease_until IS NOT NULL AND lease_until > now()
                FROM agent_invocations
                WHERE caller_agent_id=? AND invocation_id=?
                FOR UPDATE
                """, (rs, n) -> new Claim(
                rs.getString(2), 0, null, rs.getString(3), rs.getString(4), rs.getString(5)),
                caller, invocationId);
        if (existing.isEmpty()) {
            return new Claim("IN_PROGRESS", 0, null, null, null, "the same invocation is still running");
        }
        String hash = jdbc.queryForObject("""
                SELECT request_hash FROM agent_invocations WHERE caller_agent_id=? AND invocation_id=?
                """, String.class, caller, invocationId);
        if (hash == null || !hash.equals(requestHash)) {
            return new Claim("CONFLICT", 0, null, null, "IDEMPOTENCY_CONFLICT", "invocation_id was used with different input");
        }
        Claim row = existing.getFirst();
        if ("COMPLETED".equals(row.outcome()) && row.resultJson() != null) {
            return new Claim("COMPLETED", 0, null, row.resultJson(), null, null);
        }
        Boolean leaseValid = jdbc.queryForObject("""
                SELECT lease_until IS NOT NULL AND lease_until > now()
                FROM agent_invocations WHERE caller_agent_id=? AND invocation_id=?
                """, Boolean.class, caller, invocationId);
        if ("RUNNING".equals(row.outcome()) && Boolean.TRUE.equals(leaseValid)) {
            return new Claim("IN_PROGRESS", 0, null, null, null, "the same invocation is still running");
        }
        int updated = jdbc.update("""
                UPDATE agent_invocations
                SET status='RUNNING', lease_owner=?, lease_until=now() + (? * interval '1 second'),
                    attempts=attempts + 1, fencing_token=fencing_token + 1,
                    result_json=NULL, error_code=NULL, safe_message=NULL, finished_at=NULL, updated_at=now()
                WHERE caller_agent_id=? AND invocation_id=? AND request_hash=?
                  AND (
                    status IN ('FAILED', 'TIMED_OUT', 'UNKNOWN')
                    OR (status = 'RUNNING' AND (lease_until IS NULL OR lease_until <= now()))
                    OR (status = 'COMPLETED' AND result_json IS NULL)
                  )
                """, owner, leaseSeconds, caller, invocationId, requestHash);
        if (updated != 1) {
            return new Claim("IN_PROGRESS", 0, null, null, null, "the same invocation is still running");
        }
        Long token = jdbc.queryForObject("""
                SELECT fencing_token FROM agent_invocations WHERE caller_agent_id=? AND invocation_id=?
                """, Long.class, caller, invocationId);
        return new Claim("RUN", token == null ? 0 : token, owner, null, null, null);
    }

    @Override
    @Transactional
    public boolean finish(String caller, String invocationId, long fencingToken, String status, String resultJson, String errorCode, String safeMessage) {
        int updated = jdbc.update("""
                UPDATE agent_invocations
                SET status=?, result_json=?, error_code=?, safe_message=?, updated_at=now(), lease_until=NULL,
                    finished_at=CASE WHEN ? IN ('COMPLETED','FAILED','TIMED_OUT','UNKNOWN') THEN now() ELSE finished_at END
                WHERE caller_agent_id=? AND invocation_id=? AND fencing_token=?
                """, status, resultJson, errorCode, safeMessage, status, caller, invocationId, fencingToken);
        return updated == 1;
    }
}
