package com.alertflow.integration.agent;

import java.util.Properties;
import java.util.concurrent.ConcurrentHashMap;

import com.alibaba.nacos.api.NacosFactory;
import com.alibaba.nacos.api.naming.NamingService;
import com.alibaba.nacos.api.naming.listener.NamingEvent;
import com.alibaba.nacos.api.naming.pojo.Instance;
import org.springframework.stereotype.Component;

@Component
public class AgentRegistry {
    private final ConcurrentHashMap<String, Snapshot> cards = new ConcurrentHashMap<>();
    private final ConcurrentHashMap<String, String> seeds = new ConcurrentHashMap<>();
    private final long staleMillis = Long.parseLong(env("INTEGRATION_REGISTRY_STALE_SECONDS", "300")) * 1000;

    public AgentRegistry() {
        seedFromEnv();
        watchNacos();
    }

    public String cardUrl(String agentId) {
        Snapshot snapshot = cards.get(agentId);
        if (snapshot != null && System.currentTimeMillis() - snapshot.seenAt <= staleMillis) {
            return snapshot.url;
        }
        String seed = seeds.get(agentId);
        if (seed != null) {
            return seed;
        }
        throw new IllegalStateException("agent registry has no fresh endpoint");
    }

    public void replace(String agentId, String url) {
        cards.put(agentId, new Snapshot(url, System.currentTimeMillis()));
    }

    private void seedFromEnv() {
        String raw = System.getenv("INTEGRATION_AGENT_CARDS");
        if (raw == null || raw.isBlank()) {
            return;
        }
        for (String item : raw.split(",")) {
            int eq = item.indexOf('=');
            if (eq > 0) {
                String id = item.substring(0, eq).trim();
                String url = item.substring(eq + 1).trim();
                seeds.put(id, url);
                replace(id, url);
            }
        }
    }

    private void watchNacos() {
        String server = System.getenv("NACOS_SERVER_ADDR");
        if (server == null || server.isBlank()) {
            return;
        }
        try {
            Properties properties = new Properties();
            properties.setProperty("serverAddr", server);
            String user = System.getenv("NACOS_USERNAME");
            String password = System.getenv("NACOS_PASSWORD");
            if (user != null && !user.isBlank()) {
                properties.setProperty("username", user);
                properties.setProperty("password", password == null ? "" : password);
            }
            NamingService naming = NacosFactory.createNamingService(properties);
            naming.subscribe("knowledge-agent", event -> {
                if (event instanceof NamingEvent namingEvent && !namingEvent.getInstances().isEmpty()) {
                    Instance instance = namingEvent.getInstances().get(0);
                    String card = instance.getMetadata().getOrDefault("agentCardUrl", "http://" + instance.getIp() + ":" + instance.getPort());
                    replace("knowledge-agent", card);
                }
            });
        } catch (Exception ignored) {
            // 已有快照可在陈旧期内继续使用。没有快照时新调用会失败关闭。
        }
    }

    private static String env(String key, String fallback) {
        String value = System.getenv(key);
        return value == null || value.isBlank() ? fallback : value;
    }

    private record Snapshot(String url, long seenAt) {}
}
