package com.alertflow.integration.mcp;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.springframework.stereotype.Component;

@Component
public class McpClientManager {
    public record Tool(String provider, String name, String description, String schemaJson) {}

    private final ObjectMapper json = new ObjectMapper();
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).build();
    private final Map<String, String> upstreams = new ConcurrentHashMap<>();
    private final Map<String, List<Tool>> catalog = new ConcurrentHashMap<>();

    public McpClientManager() {
        String raw = System.getenv("INTEGRATION_MCP_UPSTREAMS");
        if (raw != null) {
            for (String item : raw.split(",")) {
                int eq = item.indexOf('=');
                if (eq > 0) {
                    add(item.substring(0, eq).trim(), item.substring(eq + 1).trim());
                }
            }
        }
    }

    public void remember(Tool tool) {
        catalog.computeIfAbsent(tool.provider(), key -> new java.util.ArrayList<>()).add(tool);
        upstreams.putIfAbsent(tool.provider(), "memory");
    }

    public void add(String provider, String endpoint) {
        upstreams.put(provider, endpoint);
        refresh(provider);
    }

    public List<Tool> tools() {
        List<Tool> all = new ArrayList<>();
        catalog.values().forEach(all::addAll);
        return all;
    }

    public String call(String provider, String tool, String arguments) throws Exception {
        String endpoint = upstreams.get(provider);
        if (endpoint == null) {
            throw new IllegalStateException("provider unavailable");
        }
        String body = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"" + tool + "\",\"arguments\":" + arguments + "}}";
        JsonNode response = post(endpoint, body);
        if (response.has("error")) {
            throw new IllegalStateException("provider unavailable");
        }
        return response.path("result").path("content").path(0).path("text").asText();
    }

    private void refresh(String provider) {
        try {
            JsonNode response = post(upstreams.get(provider), "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}");
            List<Tool> tools = new ArrayList<>();
            for (JsonNode tool : response.path("result").path("tools")) {
                tools.add(new Tool(provider, tool.path("name").asText(), tool.path("description").asText(""), tool.path("inputSchema").toString()));
            }
            catalog.put(provider, tools);
        } catch (Exception error) {
            catalog.remove(provider);
        }
    }

    private JsonNode post(String endpoint, String body) throws Exception {
        HttpRequest request = HttpRequest.newBuilder(URI.create(endpoint))
                .timeout(Duration.ofSeconds(3))
                .header("content-type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(body))
                .build();
        HttpResponse<String> response = http.send(request, HttpResponse.BodyHandlers.ofString());
        return json.readTree(response.body());
    }
}
