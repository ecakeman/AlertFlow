package com.alertflow.integration.bridge;

import io.grpc.Context;
import io.grpc.Contexts;
import io.grpc.ForwardingClientCall;
import io.grpc.Metadata;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.ClientCall;
import io.grpc.ClientInterceptor;
import io.grpc.MethodDescriptor;
import io.grpc.CallOptions;
import io.grpc.Channel;

// 把进入 Java 的 deadline 上下文和少量追踪头继续传给上游。
public final class HeaderForwarding implements ServerInterceptor, ClientInterceptor {
    public static final Context.Key<Metadata> INCOMING = Context.key("incoming-metadata");
    public static final Metadata.Key<String> CALLER = Metadata.Key.of("x-caller-agent", Metadata.ASCII_STRING_MARSHALLER);
    private static final String[] COPY = {"x-request-id", "traceparent", "tracestate"};

    @Override
    public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(ServerCall<ReqT, RespT> call, Metadata headers, ServerCallHandler<ReqT, RespT> next) {
        return Contexts.interceptCall(Context.current().withValue(INCOMING, headers), call, headers, next);
    }

    @Override
    public <ReqT, RespT> ClientCall<ReqT, RespT> interceptCall(MethodDescriptor<ReqT, RespT> method, CallOptions options, Channel next) {
        return new ForwardingClientCall.SimpleForwardingClientCall<>(next.newCall(method, options)) {
            @Override
            public void start(Listener<RespT> responseListener, Metadata headers) {
                Metadata incoming = INCOMING.get();
                if (incoming != null) {
                    for (String name : COPY) {
                        Metadata.Key<String> key = Metadata.Key.of(name, Metadata.ASCII_STRING_MARSHALLER);
                        String value = incoming.get(key);
                        if (value != null) {
                            headers.put(key, value);
                        }
                    }
                }
                super.start(responseListener, headers);
            }
        };
    }
}
