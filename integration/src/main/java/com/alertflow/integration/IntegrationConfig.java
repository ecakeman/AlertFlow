package com.alertflow.integration;

import com.alertflow.integration.agent.AgentRegistry;
import com.alertflow.integration.agent.CallGovernance;
import com.alertflow.integration.agent.InvocationService;
import com.alertflow.integration.agent.InvocationStore;
import com.alertflow.integration.agent.KnowledgeClient;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

@Configuration
public class IntegrationConfig {
    @Bean
    InvocationService invocationService(InvocationStore store, AgentRegistry registry, KnowledgeClient knowledge, CallGovernance governance) {
        return new InvocationService(store, registry, knowledge, governance);
    }
}
