package com.alertflow.integration.agent;

import java.util.concurrent.Callable;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import org.springframework.stereotype.Component;

// 只限制 Java 自己发出的远端调用，不参与 Episode 排队。
@Component
public class CallGovernance {
    private final Semaphore permits = new Semaphore(parse("INTEGRATION_MAX_INFLIGHT", 32));
    private final AtomicInteger failures = new AtomicInteger();
    private volatile long openUntil;

    public String call(Callable<String> action) throws Exception {
        if (System.currentTimeMillis() < openUntil) {
            throw new IllegalStateException("circuit open");
        }
        if (!permits.tryAcquire(2, TimeUnit.SECONDS)) {
            throw new IllegalStateException("bulkhead full");
        }
        try {
            String value = action.call();
            failures.set(0);
            return value;
        } catch (Exception error) {
            if (failures.incrementAndGet() >= 5) {
                openUntil = System.currentTimeMillis() + 10_000;
            }
            throw error;
        } finally {
            permits.release();
        }
    }

    private static int parse(String name, int fallback) {
        try {
            String raw = System.getenv(name);
            return raw == null || raw.isBlank() ? fallback : Integer.parseInt(raw);
        } catch (NumberFormatException error) {
            return fallback;
        }
    }
}
