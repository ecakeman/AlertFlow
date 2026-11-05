package com.alertflow.integration;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;

@SpringBootApplication
public class IntegrationApplication {
    public static void main(String[] args) {
        String http = System.getenv("INTEGRATION_HTTP_ADDR");
        if (http != null && !http.isBlank()) {
            String port = http.contains(":") ? http.substring(http.lastIndexOf(':') + 1) : http;
            System.setProperty("server.port", port);
        }
        SpringApplication.run(IntegrationApplication.class, args);
    }
}
