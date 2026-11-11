package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/api"
	"github.com/ecakeman/alertflow/backend/internal/cache"
	"github.com/ecakeman/alertflow/backend/internal/db"
	"github.com/ecakeman/alertflow/backend/internal/gateway"
	"github.com/ecakeman/alertflow/backend/internal/monitor"
	"github.com/ecakeman/alertflow/backend/internal/system"
	agentv1 "github.com/ecakeman/alertflow/gen/agent/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := env("ALERTFLOW_DATABASE_URL", "postgres://alertflow:alertflow@127.0.0.1:5432/alertflow?sslmode=disable")
	conn, err := db.Open(databaseURL)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer conn.Close()
	store := system.New(conn)
	var aside cache.Aside
	if redisURL := os.Getenv("ALERTFLOW_REDIS_URL"); redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Error("redis", "err", err)
			os.Exit(1)
		}
		aside.Client = redis.NewClient(opt)
		aside.TTL = 5 * time.Second
	}
	root, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Kubernetes 观察器默认关闭；启用但集群不可用时只记录错误，采样返回观察器错误。
	k8sEnabled, parseErr := strconv.ParseBool(env("ALERTFLOW_K8S_ENABLED", "false"))
	if parseErr != nil {
		log.Warn("invalid ALERTFLOW_K8S_ENABLED, kubernetes monitoring disabled", "value", os.Getenv("ALERTFLOW_K8S_ENABLED"))
		k8sEnabled = false
	}
	k8sObserver := monitor.NewK8sObserver(k8sEnabled, env("ALERTFLOW_K8S_NAMESPACE", "default"), log)
	go monitor.Run(root, store, 4, k8sObserver, log)
	go gateway.RunRecovery(root, store, 2*time.Second)
	if brokers := split(os.Getenv("ALERTFLOW_KAFKA_BROKERS")); len(brokers) > 0 && brokers[0] != "" {
		topic := env("ALERTFLOW_KAFKA_TOPIC", "alertflow.agent-task-ready.v1")
		go gateway.RunPublisher(root, store, brokers, topic, log)
		if target := os.Getenv("ALERTFLOW_AGENT_GRPC_TARGET"); target != "" {
			go gateway.RunConsumer(root, store, brokers, topic, "alertflow-agent-gateway", target, log)
		}
	}
	grpcServer := grpc.NewServer()
	agentv1.RegisterSystemGatewayServiceServer(grpcServer, &gateway.SystemServer{Store: store})
	lis, err := net.Listen("tcp", env("ALERTFLOW_GRPC_ADDR", ":9090"))
	if err != nil {
		log.Error("grpc listen", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Error("grpc", "err", err)
		}
	}()
	// 有界关闭：HTTP 与 gRPC 各自最多等待 shutdownTimeout，整体小于 45s terminationGracePeriod。
	const shutdownTimeout = 10 * time.Second
	var shutdownWG sync.WaitGroup
	shutdownWG.Add(2)
	go func() {
		defer shutdownWG.Done()
		<-root.Done()
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			grpcServer.Stop()
		}
	}()
	httpServer := &http.Server{
		Addr:    env("ALERTFLOW_HTTP_ADDR", ":8080"),
		Handler: (&api.Server{Store: store, Cache: aside, Log: log, Artifacts: agentArtifacts(os.Getenv("ALERTFLOW_AGENT_GRPC_TARGET"))}).Handler(),
	}
	go func() {
		defer shutdownWG.Done()
		<-root.Done()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	log.Info("alertflow listening", "http", httpServer.Addr, "grpc", lis.Addr().String())
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http", "err", err)
		os.Exit(1)
	}
	shutdownWG.Wait()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func split(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
