package com.alertflow.integration.mcp;

import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.web.reactive.function.server.RouterFunction;
import org.springframework.web.reactive.function.server.ServerResponse;

@Configuration
public class McpRoutes {
    @Bean
    RouterFunction<ServerResponse> mcp(McpGateway gateway) {
        return gateway.routes();
    }
}
