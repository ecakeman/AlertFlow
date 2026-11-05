package com.alertflow.integration.mcp;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.springframework.http.MediaType;
import org.springframework.stereotype.Component;
import org.springframework.web.reactive.function.server.RouterFunction;
import org.springframework.web.reactive.function.server.RouterFunctions;
import org.springframework.web.reactive.function.server.ServerRequest;
import org.springframework.web.reactive.function.server.ServerResponse;
import reactor.core.publisher.Mono;

@Component
public class McpGateway {
    static final Set<String> SUPPORTED = Set.of("2025-11-25", "2025-06-18", "2025-03-26");
    private final ObjectMapper json = new ObjectMapper();
    private final McpPolicy policy;
    private final McpClientManager clients;

    public McpGateway(McpPolicy policy, McpClientManager clients) {
        this.policy = policy;
        this.clients = clients;
    }

    public RouterFunction<ServerResponse> routes() {
        return RouterFunctions.route()
                .POST("/mcp", this::handle)
                .build();
    }

    public Mono<ServerResponse> handle(ServerRequest request) {
        return request.bodyToMono(String.class).defaultIfEmpty("").flatMap(body -> {
            try {
                return ServerResponse.ok().contentType(MediaType.APPLICATION_JSON).bodyValue(dispatch(request.headers().firstHeader("authorization"), body));
            } catch (Exception error) {
                return ServerResponse.status(401).contentType(MediaType.APPLICATION_JSON).bodyValue("{\"error\":\"" + error.getMessage() + "\"}");
            }
        });
    }

    public String dispatch(String authorization, String body) throws Exception {
        JsonNode rpc = json.readTree(body);
        if (!rpc.has("id")) {
            return "";
        }
        String method = rpc.path("method").asText();
        ObjectNode response = json.createObjectNode();
        response.put("jsonrpc", "2.0");
        response.set("id", rpc.get("id"));
        if ("initialize".equals(method)) {
            String version = rpc.path("params").path("protocolVersion").asText();
            if (!SUPPORTED.contains(version)) {
                response.set("error", error( -32602, "unsupported protocol version " + SUPPORTED));
                return json.writeValueAsString(response);
            }
            ObjectNode result = response.putObject("result");
            result.put("protocolVersion", version);
            result.putObject("capabilities").putObject("tools").put("listChanged", false);
            result.putObject("serverInfo").put("name", "alertflow-integration").put("version", "0.1.0");
            return json.writeValueAsString(response);
        }
        String agent = policy.agentFor(authorization);
        if ("tools/list".equals(method)) {
            ObjectNode result = response.putObject("result");
            var tools = result.putArray("tools");
            for (McpClientManager.Tool tool : clients.tools()) {
                if (policy.allowed(agent, tool.provider(), tool.name())) {
                    ObjectNode item = tools.addObject();
                    item.put("name", tool.provider() + "." + tool.name());
                    item.put("description", tool.description());
                    item.set("inputSchema", json.readTree(tool.schemaJson()));
                }
            }
            return json.writeValueAsString(response);
        }
        if ("tools/call".equals(method)) {
            String name = rpc.path("params").path("name").asText();
            int dot = name.indexOf('.');
            if (dot < 1 || !policy.allowed(agent, name.substring(0, dot), name.substring(dot + 1))) {
                response.set("error", error(-32001, "tool is not authorized"));
                return json.writeValueAsString(response);
            }
            String provider = name.substring(0, dot);
            String tool = name.substring(dot + 1);
            try {
                String text = clients.call(provider, tool, rpc.path("params").path("arguments").toString());
                ObjectNode result = response.putObject("result");
                result.putArray("content").addObject().put("type", "text").put("text", text);
                result.put("isError", false);
            } catch (Exception error) {
                response.set("error", error(-32002, "provider unavailable"));
            }
            return json.writeValueAsString(response);
        }
        response.set("error", error(-32601, "method not found"));
        return json.writeValueAsString(response);
    }

    private ObjectNode error(int code, String message) {
        ObjectNode error = json.createObjectNode();
        error.put("code", code);
        error.put("message", message);
        return error;
    }

    public static String sha256(String value) throws Exception {
        return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8)));
    }

    public Map<String, Object> empty() {
        return new LinkedHashMap<>();
    }

    public List<String> supported() {
        return new ArrayList<>(SUPPORTED);
    }

    public void ignore(IOException error) {
        throw new IllegalStateException(error);
    }
}
