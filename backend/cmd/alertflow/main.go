// Command alertflow is the single-binary server: it serves the embedded web UI
// and the JSON API (Fiber), and runs the monitoring scheduler.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ecakeman/alertflow/internal/api"
	"github.com/ecakeman/alertflow/internal/consume"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/deliver"
	"github.com/ecakeman/alertflow/internal/logger"
	"github.com/ecakeman/alertflow/internal/publish"
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

	app := srv.App()
	go func() {
		if err := app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
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
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
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
	worker := deliver.New(database, "")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("delivery worker started")
	if err := worker.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("delivery worker stopped", "error", err)
		os.Exit(1)
	}
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
