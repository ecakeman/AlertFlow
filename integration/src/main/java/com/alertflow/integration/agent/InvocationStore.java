package com.alertflow.integration.agent;

public interface InvocationStore {
    record Claim(String outcome, long fencingToken, String leaseOwner, String resultJson, String errorCode, String safeMessage) {}

    Claim claim(String caller, String invocationId, String target, String skill, String requestHash, int leaseSeconds);

    boolean finish(String caller, String invocationId, long fencingToken, String status, String resultJson, String errorCode, String safeMessage);
}
