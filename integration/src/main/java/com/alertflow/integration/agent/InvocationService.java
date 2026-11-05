package com.alertflow.integration.agent;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.HexFormat;
import java.util.Map;
import java.util.TreeMap;

import com.alertflow.gen.integration.v1.InvokeAgentRequest;
import com.alertflow.gen.integration.v1.InvokeAgentResponse;
import com.alertflow.integration.bridge.HeaderForwarding;
import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.google.protobuf.Struct;
import com.google.protobuf.util.JsonFormat;
import io.grpc.Metadata;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

public class InvocationService {
    public static final String SKILL = "knowledge-retrieval";
    private static final Logger log = LoggerFactory.getLogger(InvocationService.class);
    private final InvocationStore store;
    private final AgentRegistry registry;
    private final KnowledgeClient knowledge;
    private final CallGovernance governance;
    private final ObjectMapper json = new ObjectMapper();

    public InvocationService(InvocationStore store, AgentRegistry registry, KnowledgeClient knowledge, CallGovernance governance) {
        this.store = store;
        this.registry = registry;
        this.knowledge = knowledge;
        this.governance = governance;
    }

    public InvokeAgentResponse invoke(InvokeAgentRequest request) throws Exception {
        String caller = caller();
        if (caller.isBlank() || request.getInvocationId().isBlank() || !SKILL.equals(request.getSkillId())) {
            return failed(request, "INVALID_ARGUMENT", "caller, invocation_id and skill are required");
        }
        if (!"knowledge-agent".equals(request.getTargetAgentId())) {
            return failed(request, "UNKNOWN_AGENT", "target is not on the allow list");
        }
        String inputJson = JsonFormat.printer().omittingInsignificantWhitespace().print(request.getInput());
        String hash = sha256(inputJson);
        long timeout = request.getTimeoutMs() > 0 ? request.getTimeoutMs() : 30_000;
        int leaseSeconds = (int) Math.max(45, timeout / 1000 + 15);
        InvocationStore.Claim claim = store.claim(caller, request.getInvocationId(), request.getTargetAgentId(), SKILL, hash, leaseSeconds);
        switch (claim.outcome()) {
            case "CONFLICT" -> {
                return failed(request, "IDEMPOTENCY_CONFLICT", "invocation_id was used with different input");
            }
            case "COMPLETED" -> {
                return completed(request, claim.resultJson());
            }
            case "RUN" -> {
                // 本次领取到了租约，继续执行。
            }
            default -> {
                return inProgress(request);
            }
        }
        try {
            String card = registry.cardUrl(request.getTargetAgentId());
            Map<String, Object> input = json.readValue(inputJson, new TypeReference<>() {});
            String result = governance.call(() -> knowledge.send(card, input, Duration.ofMillis(timeout)));
            if (!store.finish(caller, request.getInvocationId(), claim.fencingToken(), "COMPLETED", result, null, null)) {
                return inProgress(request);
            }
            log.info("invocation_id={} traceparent={} status=COMPLETED", request.getInvocationId(), traceparent());
            return completed(request, result);
        } catch (Exception error) {
            String status = timedOut(error) ? "TIMED_OUT" : unknown(error) ? "UNKNOWN" : "FAILED";
            String code = status.equals("FAILED") ? "UPSTREAM" : status;
            String message = error.getMessage() == null ? status : error.getMessage();
            store.finish(caller, request.getInvocationId(), claim.fencingToken(), status, null, code, message);
            log.info("invocation_id={} traceparent={} status={}", request.getInvocationId(), traceparent(), status);
            return failed(request, code, message);
        }
    }

    private static InvokeAgentResponse inProgress(InvokeAgentRequest request) {
        return InvokeAgentResponse.newBuilder()
                .setInvocationId(request.getInvocationId())
                .setStatus("IN_PROGRESS")
                .setSafeMessage("the same invocation is still running")
                .build();
    }

    private InvokeAgentResponse completed(InvokeAgentRequest request, String result) throws Exception {
        Struct.Builder struct = Struct.newBuilder();
        if (result != null && !result.isBlank()) {
            JsonFormat.parser().merge(result, struct);
        }
        return InvokeAgentResponse.newBuilder()
                .setInvocationId(request.getInvocationId())
                .setStatus("COMPLETED")
                .setResult(struct)
                .build();
    }

    private InvokeAgentResponse failed(InvokeAgentRequest request, String code, String message) {
        return InvokeAgentResponse.newBuilder()
                .setInvocationId(request.getInvocationId())
                .setStatus("FAILED")
                .setErrorCode(code == null ? "FAILED" : code)
                .setSafeMessage(message == null ? "" : message)
                .build();
    }

    private static String traceparent() {
        Metadata headers = HeaderForwarding.INCOMING.get();
        if (headers == null) {
            return "";
        }
        String value = headers.get(Metadata.Key.of("traceparent", Metadata.ASCII_STRING_MARSHALLER));
        return value == null ? "" : value;
    }

    private static String caller() {
        Metadata headers = HeaderForwarding.INCOMING.get();
        if (headers == null) {
            return "";
        }
        String value = headers.get(HeaderForwarding.CALLER);
        return value == null ? "" : value;
    }

    static String sha256(String value) throws Exception {
        byte[] digest = MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8));
        return HexFormat.of().formatHex(digest);
    }

    private static boolean timedOut(Throwable error) {
        String text = String.valueOf(error.getMessage()).toLowerCase();
        return text.contains("timeout") || text.contains("timed out") || text.contains("deadline");
    }

    private static boolean unknown(Throwable error) {
        String text = String.valueOf(error.getMessage()).toLowerCase();
        return text.contains("unknown") || text.contains("connection reset");
    }

    public static Map<String, Object> sorted(Map<String, Object> input) {
        return new TreeMap<>(input);
    }
}
