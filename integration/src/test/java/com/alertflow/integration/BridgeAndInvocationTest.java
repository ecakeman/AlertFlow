package com.alertflow.integration;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import com.alertflow.gen.agent.v1.AgentExecutionServiceGrpc;
import com.alertflow.gen.agent.v1.SubmitTaskRequest;
import com.alertflow.gen.agent.v1.SubmitTaskResponse;
import com.alertflow.gen.agent.v1.SystemGatewayServiceGrpc;
import com.alertflow.gen.agent.v1.TaskAck;
import com.alertflow.gen.agent.v1.CompleteTaskRequest;
import com.alertflow.gen.integration.v1.InvokeAgentRequest;
import com.alertflow.gen.integration.v1.InvokeAgentResponse;
import com.alertflow.integration.agent.A2AKnowledgeClient;
import com.alertflow.integration.agent.AgentRegistry;
import com.alertflow.integration.agent.CallGovernance;
import com.alertflow.integration.agent.InvocationService;
import com.alertflow.integration.agent.MemoryInvocationStore;
import com.alertflow.integration.bridge.GrpcBridge;
import com.alertflow.integration.bridge.HeaderForwarding;
import com.alertflow.integration.mcp.McpClientManager;
import com.alertflow.integration.mcp.McpGateway;
import com.alertflow.integration.mcp.McpPolicy;
import com.google.protobuf.Struct;
import com.google.protobuf.Value;
import io.grpc.Context;
import io.grpc.Grpc;
import io.grpc.InsecureChannelCredentials;
import io.grpc.InsecureServerCredentials;
import io.grpc.ManagedChannel;
import io.grpc.Metadata;
import io.grpc.Server;
import io.grpc.Status;
import io.grpc.stub.StreamObserver;
import org.junit.jupiter.api.Test;

class BridgeAndInvocationTest {
    @Test
    void submitTaskIsAckedOnlyAfterPythonAccepts() throws Exception {
        AtomicInteger calls = new AtomicInteger();
        Server python = Grpc.newServerBuilderForPort(0, InsecureServerCredentials.create())
                .addService(new AgentExecutionServiceGrpc.AgentExecutionServiceImplBase() {
                    @Override
                    public void submitTask(SubmitTaskRequest request, StreamObserver<SubmitTaskResponse> observer) {
                        calls.incrementAndGet();
                        observer.onNext(SubmitTaskResponse.newBuilder().setAccepted(true).setDuplicate(false).setTaskId(request.getTaskId()).build());
                        observer.onCompleted();
                    }
                }).build().start();
        Server go = Grpc.newServerBuilderForPort(0, InsecureServerCredentials.create())
                .addService(new SystemGatewayServiceGrpc.SystemGatewayServiceImplBase() {
                    @Override
                    public void completeTask(CompleteTaskRequest request, StreamObserver<TaskAck> observer) {
                        observer.onNext(TaskAck.newBuilder().setAccepted(true).build());
                        observer.onCompleted();
                    }
                }).build().start();
        InvocationService unused = new InvocationService(new MemoryInvocationStore(), new AgentRegistry(), (card, input, timeout) -> "{}", new CallGovernance());
        try (GrpcBridge bridge = new GrpcBridge("127.0.0.1:" + python.getPort(), "127.0.0.1:" + go.getPort(), 0, unused)) {
            ManagedChannel channel = Grpc.newChannelBuilder("127.0.0.1:" + bridge.port(), InsecureChannelCredentials.create()).build();
            SubmitTaskResponse response = AgentExecutionServiceGrpc.newBlockingStub(channel)
                    .submitTask(SubmitTaskRequest.newBuilder().setTaskId("task-1").setEpisodeId("episode-1").build());
            assertTrue(response.getAccepted());
            assertEquals(1, calls.get());
            channel.shutdownNow();
        } finally {
            python.shutdownNow().awaitTermination(2, TimeUnit.SECONDS);
            go.shutdownNow().awaitTermination(2, TimeUnit.SECONDS);
        }
    }

    @Test
    void pythonFailureDoesNotAck() throws Exception {
        Server python = Grpc.newServerBuilderForPort(0, InsecureServerCredentials.create())
                .addService(new AgentExecutionServiceGrpc.AgentExecutionServiceImplBase() {
                    @Override
                    public void submitTask(SubmitTaskRequest request, StreamObserver<SubmitTaskResponse> observer) {
                        observer.onError(Status.UNAVAILABLE.withDescription("python down").asRuntimeException());
                    }
                }).build().start();
        Server go = Grpc.newServerBuilderForPort(0, InsecureServerCredentials.create()).build().start();
        InvocationService unused = new InvocationService(new MemoryInvocationStore(), new AgentRegistry(), (card, input, timeout) -> "{}", new CallGovernance());
        try (GrpcBridge bridge = new GrpcBridge("127.0.0.1:" + python.getPort(), "127.0.0.1:" + go.getPort(), 0, unused)) {
            ManagedChannel channel = Grpc.newChannelBuilder("127.0.0.1:" + bridge.port(), InsecureChannelCredentials.create()).build();
            assertThrows(Exception.class, () -> AgentExecutionServiceGrpc.newBlockingStub(channel)
                    .submitTask(SubmitTaskRequest.newBuilder().setTaskId("task-2").build()));
            channel.shutdownNow();
        } finally {
            python.shutdownNow();
            go.shutdownNow();
        }
    }

    @Test
    void invocationIsIdempotent() throws Exception {
        MemoryInvocationStore store = new MemoryInvocationStore();
        AgentRegistry registry = new AgentRegistry();
        registry.replace("knowledge-agent", "http://knowledge-agent:8000");
        java.util.concurrent.atomic.AtomicInteger sends = new java.util.concurrent.atomic.AtomicInteger();
        InvocationService service = new InvocationService(store, registry, (card, input, timeout) -> {
            sends.incrementAndGet();
            return "{\"schemaVersion\":\"1.0\",\"results\":[]}";
        }, new CallGovernance());
        Struct input = Struct.newBuilder().putFields("query", Value.newBuilder().setStringValue("pool").build()).build();
        InvokeAgentRequest request = InvokeAgentRequest.newBuilder()
                .setInvocationId("inv-1").setTargetAgentId("knowledge-agent").setSkillId("knowledge-retrieval").setInput(input).build();
        Metadata headers = new Metadata();
        headers.put(HeaderForwarding.CALLER, "alertflow-main-agent");
        InvokeAgentResponse first = Context.current().withValue(HeaderForwarding.INCOMING, headers).call(() -> service.invoke(request));
        InvokeAgentResponse second = Context.current().withValue(HeaderForwarding.INCOMING, headers).call(() -> service.invoke(request));
        assertEquals("COMPLETED", first.getStatus());
        assertEquals("COMPLETED", second.getStatus());
        assertEquals(1, sends.get());
        InvokeAgentRequest changed = request.toBuilder().setInput(Struct.newBuilder().putFields("query", Value.newBuilder().setStringValue("other").build()).build()).build();
        InvokeAgentResponse conflict = Context.current().withValue(HeaderForwarding.INCOMING, headers).call(() -> service.invoke(changed));
        assertEquals("IDEMPOTENCY_CONFLICT", conflict.getErrorCode());
    }

    @Test
    void mcpListAndCallAreAuthorizedSeparately() throws Exception {
        McpPolicy policy = McpPolicy.memory();
        policy.remember("main-token", "alertflow-main-agent");
        policy.remember("knowledge-token", "knowledge-agent");
        policy.grant("alertflow-main-agent", "mock", "echo");
        McpClientManager clients = new McpClientManager();
        clients.remember(new McpClientManager.Tool("mock", "echo", "echo", "{\"type\":\"object\"}"));
        clients.remember(new McpClientManager.Tool("mock", "secret", "secret", "{\"type\":\"object\"}"));
        McpGateway gateway = new McpGateway(policy, clients);
        String listed = gateway.dispatch("Bearer main-token", "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}");
        assertTrue(listed.contains("mock.echo"));
        assertFalse(listed.contains("secret"));
        String denied = gateway.dispatch("Bearer knowledge-token", "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"mock.echo\",\"arguments\":{}}}");
        assertTrue(denied.contains("not authorized"));
        assertThrows(IllegalStateException.class, () -> gateway.dispatch("Bearer nope", "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/list\"}"));
    }

    @Test
    void a2aMessageSendReadsDataPart() throws Exception {
        com.sun.net.httpserver.HttpServer http = com.sun.net.httpserver.HttpServer.create(new java.net.InetSocketAddress("127.0.0.1", 0), 0);
        int port = http.getAddress().getPort();
        http.createContext("/", exchange -> {
            String path = exchange.getRequestURI().getPath();
            byte[] body = path.endsWith("agent-card.json")
                    ? ("""
                    {"name":"knowledge-agent","description":"retrieval","version":"1.0.0",
                     "capabilities":{"streaming":false,"pushNotifications":false,"extendedAgentCard":false},
                     "defaultInputModes":["application/json"],"defaultOutputModes":["application/json"],
                     "skills":[{"id":"knowledge-retrieval","name":"knowledge-retrieval","description":"retrieval","tags":["retrieval"]}],
                     "supportedInterfaces":[{"protocolBinding":"JSONRPC","url":"http://127.0.0.1:%d/","protocolVersion":"1.0"}]}
                    """).formatted(port).getBytes()
                    : """
                    {"jsonrpc":"2.0","id":"1","result":{"message":{"role":"ROLE_AGENT","messageId":"m1","parts":[{"data":{"schemaVersion":"knowledge-retrieval-result-v1","results":[]}}]}}}
                    """.getBytes();
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, body.length);
            exchange.getResponseBody().write(body);
            exchange.close();
        });
        http.start();
        try {
            String result = new A2AKnowledgeClient("test-token").send("http://127.0.0.1:" + port, java.util.Map.of("query", "pool"), java.time.Duration.ofSeconds(5));
            assertTrue(result.contains("knowledge-retrieval-result-v1"));
        } finally {
            http.stop(0);
        }
    }

    @Test
    void lockedDependenciesArePresent() throws Exception {
        Class.forName("org.a2aproject.sdk.client.Client");
        Class.forName("com.alibaba.nacos.api.NacosFactory");
        Class.forName("org.springframework.ai.mcp.server.common.autoconfigure.McpServerAutoConfiguration");
    }
}
