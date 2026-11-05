package com.alertflow.integration.agent;

import java.util.Optional;
import java.util.concurrent.ConcurrentHashMap;

public final class MemoryInvocationStore implements InvocationStore {
    private final ConcurrentHashMap<String, Row> rows = new ConcurrentHashMap<>();

    @Override
    public Optional<Row> find(String caller, String invocationId) {
        return Optional.ofNullable(rows.get(key(caller, invocationId)));
    }

    @Override
    public void insert(Row row) {
        rows.put(key(row.caller(), row.invocationId()), row);
    }

    @Override
    public void finish(String caller, String invocationId, String status, String resultJson, String errorCode, String safeMessage) {
        rows.computeIfPresent(key(caller, invocationId), (id, row) -> new Row(
                row.caller(), row.invocationId(), row.target(), row.skill(), row.requestHash(),
                status, resultJson, errorCode, safeMessage));
    }

    private static String key(String caller, String invocationId) {
        return caller + "\n" + invocationId;
    }
}
