package com.alertflow.integration.bridge;

import java.util.concurrent.TimeUnit;

import com.alertflow.gen.agent.v1.AgentExecutionServiceGrpc;
import com.alertflow.gen.agent.v1.GetArtifactRequest;
import com.alertflow.gen.agent.v1.GetArtifactResponse;
import com.alertflow.gen.agent.v1.SubmitTaskRequest;
import com.alertflow.gen.agent.v1.SubmitTaskResponse;
import com.alertflow.gen.agent.v1.CompleteTaskRequest;
import com.alertflow.gen.agent.v1.GetEpisodeInputRequest;
import com.alertflow.gen.agent.v1.GetEpisodeInputResponse;
import com.alertflow.gen.agent.v1.HeartbeatTaskRequest;
import com.alertflow.gen.agent.v1.QueryTimelineRequest;
import com.alertflow.gen.agent.v1.QueryTimelineResponse;
import com.alertflow.gen.agent.v1.RegisterArtifactRequest;
import com.alertflow.gen.agent.v1.RegisterArtifactResponse;
import com.alertflow.gen.agent.v1.RegisterFaultDocumentRequest;
import com.alertflow.gen.agent.v1.RegisterFaultDocumentResponse;
import com.alertflow.gen.agent.v1.ReportTaskStartedRequest;
import com.alertflow.gen.agent.v1.SystemGatewayServiceGrpc;
import com.alertflow.gen.agent.v1.TaskAck;
import com.alertflow.gen.integration.v1.AgentInvocationServiceGrpc;
import com.alertflow.gen.integration.v1.InvokeAgentRequest;
import com.alertflow.gen.integration.v1.InvokeAgentResponse;
import com.alertflow.integration.agent.InvocationService;
import io.grpc.Grpc;
import io.grpc.InsecureChannelCredentials;
import io.grpc.InsecureServerCredentials;
import io.grpc.ManagedChannel;
import io.grpc.Server;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.stub.StreamObserver;
import jakarta.annotation.PreDestroy;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

// Go 和 Python 之间的两个既有服务，以及新的 InvokeAgent。Java 不改业务字段。
@Component
public class GrpcBridge implements AutoCloseable {
    private Server server;
    private ManagedChannel python;
    private ManagedChannel go;

    @Autowired
    public GrpcBridge(InvocationService invocations) {
        try {
            start(env("INTEGRATION_PYTHON_GRPC_TARGET", "127.0.0.1:9000"), env("INTEGRATION_GO_GRPC_TARGET", "127.0.0.1:9090"), port(env("INTEGRATION_GRPC_ADDR", ":9091")), invocations);
        } catch (Exception error) {
            throw new IllegalStateException(error);
        }
    }

    public GrpcBridge(String pythonTarget, String goTarget, int port, InvocationService invocations) throws Exception {
        start(pythonTarget, goTarget, port, invocations);
    }

    private void start(String pythonTarget, String goTarget, int port, InvocationService invocations) throws Exception {
        this.python = Grpc.newChannelBuilder(pythonTarget, InsecureChannelCredentials.create()).build();
        this.go = Grpc.newChannelBuilder(goTarget, InsecureChannelCredentials.create()).build();
        this.server = Grpc.newServerBuilderForPort(port, InsecureServerCredentials.create())
                .intercept(new HeaderForwarding())
                .addService(new ExecutionProxy(AgentExecutionServiceGrpc.newBlockingStub(python).withInterceptors(new HeaderForwarding())))
                .addService(new GatewayProxy(SystemGatewayServiceGrpc.newBlockingStub(go).withInterceptors(new HeaderForwarding())))
                .addService(new InvocationProxy(invocations))
                .build()
                .start();
    }

    @PreDestroy
    public void close() throws InterruptedException {
        server.shutdown();
        python.shutdown();
        go.shutdown();
        server.awaitTermination(5, TimeUnit.SECONDS);
    }

    public int port() {
        return server.getPort();
    }

    private static String env(String key, String fallback) {
        String value = System.getenv(key);
        return value == null || value.isBlank() ? fallback : value;
    }

    private static int port(String addr) {
        int colon = addr.lastIndexOf(':');
        return Integer.parseInt(colon < 0 ? addr : addr.substring(colon + 1));
    }

    private static <T> void finish(StreamObserver<T> observer, T value) {
        observer.onNext(value);
        observer.onCompleted();
    }

    private static void fail(StreamObserver<?> observer, Exception error) {
        if (error instanceof StatusRuntimeException status) {
            observer.onError(status);
            return;
        }
        observer.onError(Status.INTERNAL.withDescription(error.getMessage()).asRuntimeException());
    }

    static final class ExecutionProxy extends AgentExecutionServiceGrpc.AgentExecutionServiceImplBase {
        private final AgentExecutionServiceGrpc.AgentExecutionServiceBlockingStub python;

        ExecutionProxy(AgentExecutionServiceGrpc.AgentExecutionServiceBlockingStub python) {
            this.python = python;
        }

        @Override
        public void submitTask(SubmitTaskRequest request, StreamObserver<SubmitTaskResponse> observer) {
            try {
                finish(observer, python.submitTask(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void getArtifact(GetArtifactRequest request, StreamObserver<GetArtifactResponse> observer) {
            try {
                finish(observer, python.getArtifact(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }
    }

    static final class GatewayProxy extends SystemGatewayServiceGrpc.SystemGatewayServiceImplBase {
        private final SystemGatewayServiceGrpc.SystemGatewayServiceBlockingStub go;

        GatewayProxy(SystemGatewayServiceGrpc.SystemGatewayServiceBlockingStub go) {
            this.go = go;
        }

        @Override
        public void getEpisodeInput(GetEpisodeInputRequest request, StreamObserver<GetEpisodeInputResponse> observer) {
            try {
                finish(observer, go.getEpisodeInput(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void queryTimeline(QueryTimelineRequest request, StreamObserver<QueryTimelineResponse> observer) {
            try {
                finish(observer, go.queryTimeline(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void registerArtifact(RegisterArtifactRequest request, StreamObserver<RegisterArtifactResponse> observer) {
            try {
                finish(observer, go.registerArtifact(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void reportTaskStarted(ReportTaskStartedRequest request, StreamObserver<TaskAck> observer) {
            try {
                finish(observer, go.reportTaskStarted(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void heartbeatTask(HeartbeatTaskRequest request, StreamObserver<TaskAck> observer) {
            try {
                finish(observer, go.heartbeatTask(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void completeTask(CompleteTaskRequest request, StreamObserver<TaskAck> observer) {
            try {
                finish(observer, go.completeTask(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }

        @Override
        public void registerFaultDocument(RegisterFaultDocumentRequest request, StreamObserver<RegisterFaultDocumentResponse> observer) {
            try {
                finish(observer, go.registerFaultDocument(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }
    }

    static final class InvocationProxy extends AgentInvocationServiceGrpc.AgentInvocationServiceImplBase {
        private final InvocationService invocations;

        InvocationProxy(InvocationService invocations) {
            this.invocations = invocations;
        }

        @Override
        public void invokeAgent(InvokeAgentRequest request, StreamObserver<InvokeAgentResponse> observer) {
            try {
                finish(observer, invocations.invoke(request));
            } catch (Exception error) {
                fail(observer, error);
            }
        }
    }
}
