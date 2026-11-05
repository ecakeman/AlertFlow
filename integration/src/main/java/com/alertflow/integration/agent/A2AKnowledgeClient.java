package com.alertflow.integration.agent;

import java.net.http.HttpClient;
import java.time.Duration;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicReference;

import com.fasterxml.jackson.databind.ObjectMapper;
import org.a2aproject.sdk.A2A;
import org.a2aproject.sdk.client.Client;
import org.a2aproject.sdk.client.MessageEvent;
import org.a2aproject.sdk.client.http.JdkA2AHttpClient;
import org.a2aproject.sdk.client.config.ClientConfig;
import org.a2aproject.sdk.client.transport.jsonrpc.JSONRPCTransport;
import org.a2aproject.sdk.client.transport.jsonrpc.JSONRPCTransportConfigBuilder;
import org.a2aproject.sdk.client.transport.spi.interceptors.ClientCallContext;
import org.a2aproject.sdk.spec.AgentCard;
import org.a2aproject.sdk.spec.AgentSkill;
import org.a2aproject.sdk.spec.DataPart;
import org.a2aproject.sdk.spec.Message;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

@Component
public class A2AKnowledgeClient implements KnowledgeClient {
    private final ObjectMapper json = new ObjectMapper();
    private final String token;

    @Autowired
    public A2AKnowledgeClient() {
        this(System.getenv("KNOWLEDGE_A2A_TOKEN"));
    }

    A2AKnowledgeClient(String token) {
        this.token = token == null ? "" : token;
    }

    @Override
    public String send(String cardUrl, Map<String, Object> input, Duration timeout) throws Exception {
        Duration deadline = timeout == null ? Duration.ofSeconds(30) : timeout;
        AtomicReference<String> result = new AtomicReference<>();
        AtomicReference<Throwable> failure = new AtomicReference<>();
        ExecutorService caller = Executors.newSingleThreadExecutor(task -> {
            Thread thread = new Thread(task, "a2a-knowledge");
            thread.setDaemon(true);
            return thread;
        });
        try {
            caller.submit(() -> {
                try {
                    result.set(sendWithinCaller(cardUrl, input, deadline, failure));
                } catch (Exception error) {
                    failure.set(error);
                }
            }).get(deadline.toMillis(), TimeUnit.MILLISECONDS);
        } catch (TimeoutException error) {
            throw new IllegalStateException("timed out", error);
        } catch (ExecutionException error) {
            Throwable cause = error.getCause() == null ? error : error.getCause();
            throw new IllegalStateException(cause.getMessage(), cause);
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("timed out", error);
        } finally {
            caller.shutdownNow();
        }
        if (failure.get() != null) {
            throw new IllegalStateException(failure.get().getMessage(), failure.get());
        }
        if (result.get() == null) {
            throw new IllegalStateException("unknown: A2A response had no data part");
        }
        return result.get();
    }

    private String sendWithinCaller(String cardUrl, Map<String, Object> input, Duration deadline,
                                    AtomicReference<Throwable> failure) throws Exception {
        AgentCard card = A2A.getAgentCard(cardUrl);
        boolean skill = false;
        for (AgentSkill item : card.skills()) {
            if ("knowledge-retrieval".equals(item.id())) {
                skill = true;
            }
        }
        if (!skill) {
            throw new IllegalStateException("agent card has no knowledge-retrieval skill");
        }
        if (card.capabilities().streaming() || card.capabilities().pushNotifications()) {
            throw new IllegalStateException("knowledge agent must not advertise streaming");
        }
        AtomicReference<Object> data = new AtomicReference<>();
        ClientConfig config = new ClientConfig.Builder().setStreaming(false).build();
        Message message = Message.builder()
                .role(Message.Role.ROLE_USER)
                .messageId(UUID.randomUUID().toString())
                .parts(new DataPart(input))
                .build();
        HttpClient http = HttpClient.newBuilder()
                .version(HttpClient.Version.HTTP_1_1)
                .connectTimeout(deadline)
                .build();
        try (Client client = Client.builder(card)
                .clientConfig(config)
                .withTransport(JSONRPCTransport.class, new JSONRPCTransportConfigBuilder().httpClient(new JdkA2AHttpClient(http)))
                .addConsumer((event, ignored) -> {
                    if (event instanceof MessageEvent messageEvent) {
                        messageEvent.getMessage().parts().forEach(part -> {
                            if (part instanceof DataPart dataPart) {
                                data.set(dataPart.data());
                            }
                        });
                    }
                })
                .streamingErrorHandler(failure::set)
                .build()) {
            client.sendMessage(message, callContext(deadline));
        }
        if (failure.get() != null) {
            throw new IllegalStateException(failure.get().getMessage(), failure.get());
        }
        if (data.get() == null) {
            throw new IllegalStateException("unknown: A2A response had no data part");
        }
        return json.writeValueAsString(data.get());
    }

    private ClientCallContext callContext(Duration timeout) {
        if (token.isBlank()) {
            throw new IllegalStateException("KNOWLEDGE_A2A_TOKEN is required");
        }
        long millis = timeout == null ? 30_000 : timeout.toMillis();
        return new ClientCallContext(Map.of(), Map.of(
                "Authorization", "Bearer " + token,
                "X-Knowledge-Timeout-Ms", Long.toString(millis)));
    }
}
