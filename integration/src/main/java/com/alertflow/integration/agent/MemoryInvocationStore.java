package com.alertflow.integration.agent;

import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;

public final class MemoryInvocationStore implements InvocationStore {
    private static final class Slot {
        final String requestHash;
        String status;
        String resultJson;
        String errorCode;
        String safeMessage;
        String leaseOwner;
        long fencingToken;
        long leaseUntil;

        Slot(String requestHash) {
            this.requestHash = requestHash;
        }
    }

    private final ConcurrentHashMap<String, Slot> rows = new ConcurrentHashMap<>();

    @Override
    public synchronized Claim claim(String caller, String invocationId, String target, String skill, String requestHash, int leaseSeconds) {
        String key = caller + "\n" + invocationId;
        Slot slot = rows.get(key);
        String owner = UUID.randomUUID().toString();
        long until = System.currentTimeMillis() + leaseSeconds * 1000L;
        if (slot == null) {
            Slot created = new Slot(requestHash);
            created.status = "RUNNING";
            created.leaseOwner = owner;
            created.fencingToken = 1;
            created.leaseUntil = until;
            rows.put(key, created);
            return new Claim("RUN", 1, owner, null, null, null);
        }
        if (!slot.requestHash.equals(requestHash)) {
            return new Claim("CONFLICT", 0, null, null, "IDEMPOTENCY_CONFLICT", "invocation_id was used with different input");
        }
        if ("COMPLETED".equals(slot.status) && slot.resultJson != null) {
            return new Claim("COMPLETED", slot.fencingToken, slot.leaseOwner, slot.resultJson, null, null);
        }
        if ("RUNNING".equals(slot.status) && slot.leaseUntil > System.currentTimeMillis()) {
            return new Claim("IN_PROGRESS", 0, null, null, null, "the same invocation is still running");
        }
        boolean reclaimable = "FAILED".equals(slot.status) || "TIMED_OUT".equals(slot.status) || "UNKNOWN".equals(slot.status)
                || ("RUNNING".equals(slot.status) && slot.leaseUntil <= System.currentTimeMillis())
                || ("COMPLETED".equals(slot.status) && slot.resultJson == null);
        if (!reclaimable) {
            return new Claim("IN_PROGRESS", 0, null, null, null, "the same invocation is still running");
        }
        slot.status = "RUNNING";
        slot.leaseOwner = owner;
        slot.fencingToken += 1;
        slot.leaseUntil = until;
        slot.resultJson = null;
        slot.errorCode = null;
        slot.safeMessage = null;
        return new Claim("RUN", slot.fencingToken, owner, null, null, null);
    }

    @Override
    public synchronized boolean finish(String caller, String invocationId, long fencingToken, String status, String resultJson, String errorCode, String safeMessage) {
        Slot slot = rows.get(caller + "\n" + invocationId);
        if (slot == null || slot.fencingToken != fencingToken) {
            return false;
        }
        slot.status = status;
        slot.resultJson = resultJson;
        slot.errorCode = errorCode;
        slot.safeMessage = safeMessage;
        slot.leaseUntil = 0;
        return true;
    }
}
