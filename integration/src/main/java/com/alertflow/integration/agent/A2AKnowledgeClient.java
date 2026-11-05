package com.alertflow.integration.agent;

import java.net.http.HttpClient;
import java.time.Duration;
import java.util.Map;
import java.util.UUID;
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
import org.springframework.stereotype.Component;

@Component
public class A2AKnowledgeClient implements KnowledgeClient {
    private final ObjectMapper json = new ObjectMapper();

    @Override
    public String send(String cardUrl, Map<String, Object> input, Duration timeout) throws Exception {
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
        AtomicReference<Throwable> failure = new AtomicReference<>();
        ClientConfig config = new ClientConfig.Builder().setStreaming(false).build();
        Message message = Message.builder()
                .role(Message.Role.ROLE_USER)
                .messageId(UUID.randomUUID().toString())
                .parts(new DataPart(input))
                .build();
        HttpClient http = HttpClient.newBuilder()
                .version(HttpClient.Version.HTTP_1_1)
                .connectTimeout(timeout == null ? Duration.ofSeconds(30) : timeout)
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
            client.sendMessage(message, callContext());
        }
        if (failure.get() != null) {
            throw new IllegalStateException(failure.get().getMessage(), failure.get());
        }
        if (data.get() == null) {
            throw new IllegalStateException("unknown: A2A response had no data part");
        }
        return json.writeValueAsString(data.get());
    }

    private static ClientCallContext callContext() {
        String token = System.getenv("KNOWLEDGE_A2A_TOKEN");
        if (token == null || token.isBlank()) {
            return new ClientCallContext(Map.of(), Map.of());
        }
        return new ClientCallContext(Map.of(), Map.of("Authorization", "Bearer " + token));
    }
}
