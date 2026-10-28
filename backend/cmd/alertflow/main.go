// Command alertflow is the single-binary server: it serves the embedded web UI
// and the JSON API (Gin), and runs the monitoring scheduler.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ecakeman/alertflow/internal/api"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/consume"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/deliver"
	"github.com/ecakeman/alertflow/internal/logger"
	"github.com/ecakeman/alertflow/internal/notify"
	"github.com/ecakeman/alertflow/internal/publish"
	"github.com/ecakeman/alertflow/internal/query"
	"github.com/ecakeman/alertflow/internal/state"
)

func main() {
	logger.Init()

	var (
		addr      string
		configDir string
	)
	flag.StringVar(&addr, "addr", envOr("ALERTFLOW_ADDR", ":8080"), "HTTP listen address")
	flag.StringVar(&configDir, "config-dir", "", "config directory (default: $ALERTFLOW_CONFIG_DIR or ./config)")
	flag.Parse()

	switch envOr("ALERTFLOW_ROLE", "server") {
	case "server":
		runServer(addr, configDir)
	case "publisher":
		runPublisher()
	case "consumer":
		runConsumer()
	case "delivery":
		runDelivery()
	default:
		slog.Error("ALERTFLOW_ROLE must be server, publisher, consumer, or delivery")
		os.Exit(1)
	}
}

func runServer(addr, configDir string) {
	dir := resolveConfigDir(configDir)
	slog.Info("starting alertflow", "config_dir", dir, "addr", addr)

	srv, err := api.New(dir)
	if err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer srv.Close()

	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()
	slog.Info("listening", "addr", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}

func runPublisher() {
	database := openRoleDB()
	defer database.Close()
	pub, err := publish.New(database, os.Getenv("ALERTFLOW_KAFKA_BROKERS"), publish.Topic, "")
	if err != nil {
		slog.Error("publisher", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("publisher started", "topic", publish.Topic)
	if err := pub.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("publisher stopped", "error", err)
		os.Exit(1)
	}
}

func runConsumer() {
	database := openRoleDB()
	defer database.Close()
	cons, err := consume.New(database, os.Getenv("ALERTFLOW_KAFKA_BROKERS"), publish.Topic, "")
	if err != nil {
		slog.Error("consumer", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("consumer started", "topic", publish.Topic)
	if err := cons.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("consumer stopped", "error", err)
		os.Exit(1)
	}
}

func runDelivery() {
	database := openRoleDB()
	defer database.Close()
	registerAgentSender(database, resolveConfigDir(""))
	worker := deliver.New(database, "")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("delivery worker started")
	if err := worker.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("delivery worker stopped", "error", err)
		os.Exit(1)
	}
}

// registerAgentSender installs the Agent-only channel for this process.
//
// The diagnostic context must describe the services as they are configured
// now, so the sender reloads config.yaml on every delivery instead of freezing
// whatever was on disk at startup. A reload that fails (the file is being
// replaced atomically mid-read) falls back to the last good snapshot, which is
// stale by at most one config edit rather than for the life of the process.
func registerAgentSender(database *db.DB, dir string) {
	if err := config.EnsureDir(dir); err != nil {
		slog.Error("agent delivery: config directory unusable", "dir", dir, "error", err)
		return
	}
	current, err := config.Load(dir)
	if err != nil {
		slog.Warn("agent delivery: config not readable yet; context will be sparse", "dir", dir, "error", err)
		current = &config.Config{}
	}
	live := &liveConfig{dir: dir, current: current}
	notify.Register(notify.AgentWebhookType, notify.AgentSender{
		Q: query.New(database, live.get),
	})
	slog.Info("agent delivery: registered", "type", notify.AgentWebhookType, "config_dir", dir)
}

// liveConfig serves the newest readable config snapshot.
type liveConfig struct {
	dir string

	mu      sync.RWMutex
	current *config.Config
}

func (c *liveConfig) get() *config.Config {
	if loaded, err := config.Load(c.dir); err == nil {
		c.mu.Lock()
		c.current = loaded
		c.mu.Unlock()
		return loaded
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

func openRoleDB() *db.DB {
	database, err := db.Open(os.Getenv("ALERTFLOW_DATABASE_URL"))
	if err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
	return database
}

// resolveConfigDir picks the config directory (flag > env > saved state > default)
// and persists the choice so restarts without a flag stay consistent.
func resolveConfigDir(flagDir string) string {
	if flagDir != "" {
		_ = state.Save(state.State{ConfigDir: flagDir})
		return flagDir
	}
	if env := os.Getenv("ALERTFLOW_CONFIG_DIR"); env != "" {
		_ = state.Save(state.State{ConfigDir: env})
		return env
	}
	if st := state.Load(); st.ConfigDir != "" {
		return st.ConfigDir
	}
	return "./config"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
