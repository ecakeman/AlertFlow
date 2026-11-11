package com.alertflow.integration.agent;

import java.time.Duration;
import java.util.Map;

public interface KnowledgeClient {
    String send(String cardUrl, Map<String, Object> input, Duration timeout) throws Exception;
}
