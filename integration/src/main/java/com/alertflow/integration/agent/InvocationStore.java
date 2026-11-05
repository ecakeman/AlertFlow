package com.alertflow.integration.agent;

import java.util.Optional;

public interface InvocationStore {
    record Row(String caller, String invocationId, String target, String skill, String requestHash,
               String status, String resultJson, String errorCode, String safeMessage) {}

    Optional<Row> find(String caller, String invocationId);

    void insert(Row row);

    void finish(String caller, String invocationId, String status, String resultJson, String errorCode, String safeMessage);
}
