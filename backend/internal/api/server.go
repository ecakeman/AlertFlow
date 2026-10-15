// Package api wires the HTTP server (Fiber): routing, middleware and handlers.
// The Server owns the live config snapshot, database and scheduler, and applies
// config changes copy-on-write under a lock.
package api

import (
	"context"
	"crypto/subtle"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	recovermw "github.com/gofiber/fiber/v3/middleware/recover"

	"github.com/ecakeman/alertflow/internal/cache"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/monitor"
	"github.com/ecakeman/alertflow/internal/notify"
	"github.com/ecakeman/alertflow/internal/query"
	"github.com/ecakeman/alertflow/internal/state"
	"github.com/ecakeman/alertflow/internal/web"
)

// maxBodyBytes bounds request bodies (config imports can be up to ~20 MiB).
const maxBodyBytes = 32 << 20

// cacheInvalidateTimeout bounds the post-check cache invalidation, which runs
// on the scheduler goroutine and must not delay the next check.
const cacheInvalidateTimeout = 2 * time.Second

// Server holds all runtime state and serves the HTTP API + embedded SPA.
type Server struct {
	mu         sync.RWMutex
	configDir  string
	cfg        *config.Config
	database   *db.DB
	sched      *monitor.Scheduler
	dispatcher *notify.Dispatcher
	// queries is the single composition layer for reads. Handlers and the
	// diagnostic-context builder both go through it; it holds a live config
	// accessor rather than a snapshot.
	queries *query.Service
	// redis is the read cache shared with queries. Nil when REDIS_URL is unset.
	redis *cache.Store

	app    *fiber.App
	webFS  fs.FS
	logins *loginLimiter
	// agentToken is ALERTFLOW_AGENT_TOKEN. Empty means the internal agent routes reject every request.
	agentToken string
	// agentCallbackToken is ALERTFLOW_AGENT_CALLBACK_TOKEN: the bearer the Agent
	// uses to push a run result to POST /webhooks/agent. Empty disables that edge.
	agentCallbackToken string
	// agentBaseURL/agentAPIToken point at the diagnostic agent's API (AGENT_BASE_URL /
	// AGENT_API_TOKEN). Only the legacy synchronous chat proxy still uses them;
	// reports are read from this server's own database.
	agentBaseURL  string
	agentAPIToken string
	stop          chan struct{}
}

// New opens the database and config for configDir, builds the Fiber app, starts
// monitoring and retention, and returns a ready Server.
func New(configDir string) (*Server, error) {
	configDir = filepath.Clean(configDir)
	if err := config.EnsureDir(configDir); err != nil {
		return nil, err
	}
	database, err := db.Open(os.Getenv("ALERTFLOW_DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(configDir)
	if err != nil {
		database.Close()
		return nil, err
	}

	dispatcher := notify.NewDispatcher(database)
	s := &Server{
		configDir:     configDir,
		cfg:           cfg,
		database:      database,
		sched:         monitor.New(database),
		dispatcher:    dispatcher,
		webFS:              web.FS(),
		logins:             newLoginLimiter(),
		agentToken:         os.Getenv("ALERTFLOW_AGENT_TOKEN"),
		agentCallbackToken: os.Getenv("ALERTFLOW_AGENT_CALLBACK_TOKEN"),
		agentBaseURL:       os.Getenv("AGENT_BASE_URL"),
		agentAPIToken:      os.Getenv("AGENT_API_TOKEN"),
		stop:               make(chan struct{}),
	}
	// The read cache is an enhancement: an unusable REDIS_URL is logged and the
	// API serves straight from PostgreSQL rather than refusing to start.
	redisCache, cacheErr := cache.New(os.Getenv("REDIS_URL"))
	if cacheErr != nil {
		slog.Error("redis: ignoring unusable REDIS_URL; serving without a read cache", "error", cacheErr)
		redisCache = nil
	}

	s.queries = query.New(database, s.config)
	s.queries.Cache = redisCache
	s.redis = redisCache
	s.sched.OnCheckStored = s.invalidateCachedReads
	// The Agent-only channel needs the read layer, so it cannot be built by
	// notify.init(). Registering it here also makes the "send test" button work
	// for an agent_webhook integration.
	notify.Register(notify.AgentWebhookType, notify.AgentSender{Q: s.queries})
	if redisCache.Enabled() {
		if err := redisCache.Ping(context.Background()); err != nil {
			slog.Warn("redis: not reachable, serving without a read cache", "error", err)
		} else {
			slog.Info("redis: read cache enabled")
		}
	}
	s.app = fiber.New(fiber.Config{
		AppName:      "AlertFlow",
		BodyLimit:    maxBodyBytes,
		ErrorHandler: errorHandler,
	})
	s.app.Use(recovermw.New())
	s.app.Use(s.requestLogger)
	s.routes()

	s.sched.Sync(cfg.Services)
	go s.retentionLoop()
	return s, nil
}

// App returns the Fiber app (for Listen / graceful shutdown in main).
func (s *Server) App() *fiber.App { return s.app }

// Close stops background work and closes the database.
func (s *Server) Close() {
	close(s.stop)
	s.scheduler().Stop()
	_ = s.redis.Close()
	s.mu.Lock()
	s.database.Close()
	s.mu.Unlock()
}

// routes registers every endpoint. Auth/role are applied as per-route middleware
// so each route's requirements are explicit.
func (s *Server) routes() {
	app := s.app
	auth, admin := s.authMW, s.adminMW

	// Public (no auth).
	app.Get("/api/setup/status", s.handleSetupStatus)
	app.Post("/api/setup", s.handleSetup)
	app.Post("/api/auth/login", s.handleLogin)
	app.Post("/api/auth/logout", s.handleLogout)
	// The agent's inbound edge: its own token, body limit and idempotency key.
	app.Post("/webhooks/agent", s.handleAgentCallback)

	// Authenticated.
	app.Get("/api/auth/me", auth, s.handleMe)
	app.Get("/api/services", auth, s.handleListServices)
	app.Get("/api/services/:id/metrics", auth, s.handleServiceMetrics)
	app.Get("/api/services/:id/checks", auth, s.handleServiceChecks)
	app.Get("/api/settings", auth, s.handleGetSettings)
	app.Get("/api/dashboard/stats", auth, s.handleDashboardStats)
	app.Get("/api/incidents", auth, s.handleListIncidents)
	app.Get("/api/incidents/:id", auth, s.handleGetIncident)
	app.Post("/api/incidents/:id/comments", auth, s.handleAddIncidentComment)
	app.Get("/api/incidents/:id/deliveries", auth, s.handleListDeliveries)
	app.Get("/api/incidents/:id/agent-report", auth, s.handleIncidentAgentReport)
	// Async chat: queue a question, then poll its request. The synchronous
	// POST /agent-chat below stays until the async flow is verified end to end.
	app.Get("/api/incidents/:id/agent-chat/requests", auth, s.handleListAgentChatRequests)
	app.Post("/api/incidents/:id/agent-chat/requests", auth, s.handleCreateAgentChatRequest)
	app.Get("/api/incidents/:id/agent-chat/requests/:requestId", auth, s.handleGetAgentChatRequest)
	app.Post("/api/incidents/:id/agent-chat", auth, s.handleIncidentAgentChat)

	// Admin only.
	app.Post("/api/services", auth, admin, s.handleCreateService)
	app.Patch("/api/services/layout", auth, admin, s.handleUpdateLayout)
	app.Put("/api/services/:id", auth, admin, s.handleUpdateService)
	app.Delete("/api/services/:id", auth, admin, s.handleDeleteService)
	app.Post("/api/services/:id/check", auth, admin, s.handleCheckNow)
	app.Post("/api/incidents", auth, admin, s.handleCreateIncident)
	app.Put("/api/incidents/:id", auth, admin, s.handleUpdateIncident)
	app.Delete("/api/incidents/:id", auth, admin, s.handleDeleteIncident)
	app.Post("/api/services/:id/image", auth, admin, s.handleUploadImage)
	app.Delete("/api/services/:id/image", auth, admin, s.handleDeleteImage)
	app.Put("/api/settings", auth, admin, s.handleUpdateSettings)
	app.Put("/api/settings/config-path", auth, admin, s.handleConfigPath)
	app.Get("/api/config", auth, admin, s.handleGetRawConfig)
	app.Put("/api/config", auth, admin, s.handlePutRawConfig)
	app.Get("/api/users", auth, admin, s.handleListUsers)
	app.Post("/api/users", auth, admin, s.handleCreateUser)
	app.Put("/api/users/:id", auth, admin, s.handleUpdateUser)
	app.Delete("/api/users/:id", auth, admin, s.handleDeleteUser)
	app.Get("/api/config/export", auth, admin, s.handleExport)
	app.Post("/api/config/import", auth, admin, s.handleImport)
	app.Get("/api/integrations", auth, admin, s.handleListIntegrations)
	app.Post("/api/integrations", auth, admin, s.handleCreateIntegration)
	app.Put("/api/integrations/:id", auth, admin, s.handleUpdateIntegration)
	app.Delete("/api/integrations/:id", auth, admin, s.handleDeleteIntegration)
	app.Post("/api/integrations/:id/test", auth, admin, s.handleTestIntegration)
	app.Post("/api/deliveries/:id/retry", auth, admin, s.handleRetryDelivery)

	agent := s.agentAuth
	app.Get("/internal/agent/v1/config", agent, s.handleAgentRuntimeConfig)
	app.Get("/internal/agent/v1/incidents", agent, s.handleListIncidents)
	app.Get("/internal/agent/v1/incidents/:id", agent, s.handleGetIncident)
	app.Get("/internal/agent/v1/services/:id/metrics", agent, s.handleServiceMetrics)
	app.Get("/internal/agent/v1/services/:id/checks", agent, s.handleServiceChecks)

	// Images (authenticated; the handler gates itself rather than via middleware
	// so a missing cookie renders as a 401 body instead of a redirect).
	app.Get("/images/:file", s.handleServeImage)

	// SPA fallback — must be registered last.
	app.Use(s.serveSPA)
}

// serveSPA serves the embedded SPA, falling back to index.html for unknown
// client-side routes. Unmatched /api paths return a JSON 404 instead.
func (s *Server) serveSPA(c fiber.Ctx) error {
	if strings.HasPrefix(c.Path(), "/api/") {
		return fiber.NewError(fiber.StatusNotFound, "not found")
	}
	// /webhooks/* is machine-facing too: an unmatched path must not fall through
	// to the SPA and answer a webhook with index.html.
	if strings.HasPrefix(c.Path(), "/webhooks/") {
		return fiber.NewError(fiber.StatusNotFound, "not found")
	}
	if strings.HasPrefix(c.Path(), "/internal/") {
		if err := s.requireAgent(c); err != nil {
			return err
		}
		if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead {
			return fiber.NewError(fiber.StatusMethodNotAllowed, "method not allowed")
		}
		return fiber.NewError(fiber.StatusNotFound, "not found")
	}
	p := strings.TrimPrefix(path.Clean(c.Path()), "/")
	if p == "" {
		return s.sendIndex(c)
	}
	data, err := fs.ReadFile(s.webFS, p)
	if err != nil {
		return s.sendIndex(c)
	}
	if strings.HasPrefix(p, "assets/") {
		c.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if ext := strings.TrimPrefix(path.Ext(p), "."); ext != "" {
		c.Type(ext)
	}
	return c.Send(data)
}

// bearerMatch compares an Authorization header against a token in constant
// time. A malformed or differently-sized header still burns a comparison, so
// the response time does not leak the token's length.
func bearerMatch(header, token string) bool {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		subtle.ConstantTimeCompare([]byte(token), []byte(token))
		return false
	}
	got := header[len(prefix):]
	if len(got) != len(token) {
		subtle.ConstantTimeCompare([]byte(token), []byte(token))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (s *Server) requireAgent(c fiber.Ctx) error {
	if s.agentToken == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "agent token is not configured")
	}
	if !bearerMatch(c.Get("Authorization"), s.agentToken) {
		return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
	}
	return nil
}

func (s *Server) agentAuth(c fiber.Ctx) error {
	if err := s.requireAgent(c); err != nil {
		return err
	}
	return c.Next()
}

func (s *Server) sendIndex(c fiber.Ctx) error {
	data, err := fs.ReadFile(s.webFS, "index.html")
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "web UI not built")
	}
	c.Set("Cache-Control", "no-cache")
	c.Type("html")
	return c.Send(data)
}

// --- Concurrency-safe accessors ------------------------------------------------

func (s *Server) config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) conn() *db.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.database
}

// query returns the read-composition service. It is immutable once built (it
// reads the live config through a function), so it needs no lock of its own.
func (s *Server) query() *query.Service { return s.queries }

// invalidateCachedReads retires the cached views a freshly stored check can
// change. It runs on the scheduler goroutine right after the check row commits,
// so it is bounded and never blocks the next cycle.
func (s *Server) invalidateCachedReads(serviceID string) {
	if s.queries == nil || s.queries.Cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheInvalidateTimeout)
	defer cancel()
	s.queries.InvalidateService(ctx, serviceID)
}

func (s *Server) scheduler() *monitor.Scheduler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sched
}

func (s *Server) dispatch() *notify.Dispatcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dispatcher
}

func (s *Server) dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configDir
}

// updateConfig applies a mutation to a config clone, persists it atomically,
// swaps it in and re-syncs the scheduler — all under the write lock.
func (s *Server) updateConfig(fn func(*config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := s.cfg.Clone()
	if err := fn(clone); err != nil {
		return err
	}
	if err := config.Save(s.configDir, clone); err != nil {
		return err
	}
	s.cfg = clone
	s.sched.Sync(clone.Services)
	return nil
}

// switchConfigDir points the server at a new config directory and reloads YAML.
// Runtime rows stay in the process PostgreSQL database from ALERTFLOW_DATABASE_URL;
// switching directories does not open another database.
func (s *Server) switchConfigDir(dir string) error {
	dir = filepath.Clean(dir)
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.configDir = dir
	s.cfg = cfg
	sched := s.sched
	s.mu.Unlock()

	sched.Sync(cfg.Services)
	_ = state.Save(state.State{ConfigDir: dir})
	return nil
}

// retentionLoop periodically deletes old checks and expired sessions.
func (s *Server) retentionLoop() {
	s.runRetention()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.runRetention()
		}
	}
}

func (s *Server) runRetention() {
	s.mu.RLock()
	days := s.cfg.Settings.Check.RetentionDays
	database := s.database
	s.mu.RUnlock()
	if days < 1 {
		days = config.DefaultRetentionDays
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	if n, err := database.DeleteOlderThan(cutoff); err != nil {
		slog.Error("retention: delete checks", "error", err)
	} else if n > 0 {
		slog.Info("retention: removed old checks", "count", n)
	}
	if err := database.DeleteExpiredSessions(time.Now().Unix()); err != nil {
		slog.Error("retention: delete sessions", "error", err)
	}
	// A question the agent never answered must not sit at "pending" forever.
	if n, err := database.ExpireStaleChatRequests(time.Now().Unix()); err != nil {
		slog.Error("retention: expire chat requests", "error", err)
	} else if n > 0 {
		slog.Info("retention: expired unanswered chat requests", "count", n)
	}
}
