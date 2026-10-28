// Package api wires the HTTP server (Gin): routing, middleware and handlers.
// The Server owns the live config snapshot, database and scheduler, and applies
// config changes copy-on-write under a lock.
package api

import (
	"context"
	"crypto/subtle"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/cache"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/diagnostics"
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
	diag       *diagnostics.Collector
	dispatcher *notify.Dispatcher
	// queries is the single composition layer for reads. Handlers and the
	// diagnostic-context builder both go through it; it holds a live config
	// accessor rather than a snapshot.
	queries *query.Service
	// redis is the read cache shared with queries. Nil when REDIS_URL is unset.
	redis *cache.Store

	app    *gin.Engine
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

// New opens the database and config for configDir, builds the Gin engine, starts
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
		configDir:          configDir,
		cfg:                cfg,
		database:           database,
		sched:              monitor.New(database),
		diag:               diagnostics.New(database),
		dispatcher:         dispatcher,
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
	s.newEngine()

	s.sched.Sync(cfg.Services)
	s.diag.Sync(cfg.Services)
	go s.retentionLoop()
	return s, nil
}

// Handler is the HTTP handler. main serves it with net/http.
func (s *Server) Handler() http.Handler { return s.app }

// newEngine builds the Gin engine and registers routes. Tests call it instead of New.
func (s *Server) newEngine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	// A known path with the wrong method must reach NoRoute, same as Fiber.
	// /internal/ checks the agent token before it chooses 405 or 404.
	engine.HandleMethodNotAllowed = false
	// Keep Fiber's behaviour: an unmatched path is 404 JSON or the SPA, not a redirect.
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	engine.Use(recoverJSON, s.requestLogger)
	s.app = engine
	s.routes()
	return engine
}

// Close stops background work and closes the database.
func (s *Server) Close() {
	close(s.stop)
	s.scheduler().Stop()
	if s.diag != nil {
		s.diag.Stop()
	}
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
	app.GET("/api/setup/status", adapt(s.handleSetupStatus))
	app.POST("/api/setup", adapt(s.handleSetup))
	app.POST("/api/auth/login", adapt(s.handleLogin))
	app.POST("/api/auth/logout", adapt(s.handleLogout))
	// The agent's inbound edge: its own token, body limit and idempotency key.
	app.POST("/webhooks/agent", adapt(s.handleAgentCallback))

	// Authenticated.
	app.GET("/api/auth/me", auth, adapt(s.handleMe))
	app.GET("/api/services", auth, adapt(s.handleListServices))
	app.GET("/api/services/:id/metrics", auth, adapt(s.handleServiceMetrics))
	app.GET("/api/services/:id/checks", auth, adapt(s.handleServiceChecks))
	app.GET("/api/settings", auth, adapt(s.handleGetSettings))
	app.GET("/api/dashboard/stats", auth, adapt(s.handleDashboardStats))
	app.GET("/api/incidents", auth, adapt(s.handleListIncidents))
	app.GET("/api/incidents/:id", auth, adapt(s.handleGetIncident))
	app.POST("/api/incidents/:id/comments", auth, adapt(s.handleAddIncidentComment))
	app.GET("/api/incidents/:id/deliveries", auth, adapt(s.handleListDeliveries))
	app.GET("/api/incidents/:id/agent-report", auth, adapt(s.handleIncidentAgentReport))
	// Async chat: queue a question, then poll its request. The synchronous
	// POST /agent-chat below stays until the async flow is verified end to end.
	app.GET("/api/incidents/:id/agent-chat/requests", auth, adapt(s.handleListAgentChatRequests))
	app.POST("/api/incidents/:id/agent-chat/requests", auth, adapt(s.handleCreateAgentChatRequest))
	app.GET("/api/incidents/:id/agent-chat/requests/:requestId", auth, adapt(s.handleGetAgentChatRequest))
	app.POST("/api/incidents/:id/agent-chat", auth, adapt(s.handleIncidentAgentChat))

	// Admin only.
	app.POST("/api/services", auth, admin, adapt(s.handleCreateService))
	app.PATCH("/api/services/layout", auth, admin, adapt(s.handleUpdateLayout))
	app.PUT("/api/services/:id", auth, admin, adapt(s.handleUpdateService))
	app.DELETE("/api/services/:id", auth, admin, adapt(s.handleDeleteService))
	app.POST("/api/services/:id/check", auth, admin, adapt(s.handleCheckNow))
	app.POST("/api/incidents", auth, admin, adapt(s.handleCreateIncident))
	app.PUT("/api/incidents/:id", auth, admin, adapt(s.handleUpdateIncident))
	app.DELETE("/api/incidents/:id", auth, admin, adapt(s.handleDeleteIncident))
	app.POST("/api/services/:id/image", auth, admin, adapt(s.handleUploadImage))
	app.DELETE("/api/services/:id/image", auth, admin, adapt(s.handleDeleteImage))
	app.PUT("/api/settings", auth, admin, adapt(s.handleUpdateSettings))
	app.PUT("/api/settings/config-path", auth, admin, adapt(s.handleConfigPath))
	app.GET("/api/config", auth, admin, adapt(s.handleGetRawConfig))
	app.PUT("/api/config", auth, admin, adapt(s.handlePutRawConfig))
	app.GET("/api/users", auth, admin, adapt(s.handleListUsers))
	app.POST("/api/users", auth, admin, adapt(s.handleCreateUser))
	app.PUT("/api/users/:id", auth, admin, adapt(s.handleUpdateUser))
	app.DELETE("/api/users/:id", auth, admin, adapt(s.handleDeleteUser))
	app.GET("/api/config/export", auth, admin, adapt(s.handleExport))
	app.POST("/api/config/import", auth, admin, adapt(s.handleImport))
	app.GET("/api/integrations", auth, admin, adapt(s.handleListIntegrations))
	app.POST("/api/integrations", auth, admin, adapt(s.handleCreateIntegration))
	app.PUT("/api/integrations/:id", auth, admin, adapt(s.handleUpdateIntegration))
	app.DELETE("/api/integrations/:id", auth, admin, adapt(s.handleDeleteIntegration))
	app.POST("/api/integrations/:id/test", auth, admin, adapt(s.handleTestIntegration))
	app.POST("/api/deliveries/:id/retry", auth, admin, adapt(s.handleRetryDelivery))

	agent := s.agentAuth
	app.GET("/internal/agent/v1/config", agent, adapt(s.handleAgentRuntimeConfig))
	app.GET("/internal/agent/v1/incidents", agent, adapt(s.handleListIncidents))
	app.GET("/internal/agent/v1/incidents/:id", agent, adapt(s.handleGetIncident))
	app.GET("/internal/agent/v1/services/:id/metrics", agent, adapt(s.handleServiceMetrics))
	app.GET("/internal/agent/v1/services/:id/checks", agent, adapt(s.handleServiceChecks))

	// Images (authenticated; the handler gates itself rather than via middleware
	// so a missing cookie renders as a 401 body instead of a redirect).
	app.GET("/images/:file", adapt(s.handleServeImage))

	// SPA fallback — must be registered last. Wrong methods land here too, so
	// /api and /webhooks stay JSON 404 and /internal/ still checks the token.
	app.NoRoute(adapt(s.serveSPA))
}

// serveSPA serves the embedded SPA, falling back to index.html for unknown
// client-side routes. Unmatched /api paths return a JSON 404 instead.
func (s *Server) serveSPA(c *gin.Context) error {
	p := c.Request.URL.Path
	if strings.HasPrefix(p, "/api/") {
		return fail(http.StatusNotFound, "not found")
	}
	// /webhooks/* is machine-facing too: an unmatched path must not fall through
	// to the SPA and answer a webhook with index.html.
	if strings.HasPrefix(p, "/webhooks/") {
		return fail(http.StatusNotFound, "not found")
	}
	if strings.HasPrefix(p, "/internal/") {
		if err := s.requireAgent(c); err != nil {
			return err
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			return fail(http.StatusMethodNotAllowed, "method not allowed")
		}
		return fail(http.StatusNotFound, "not found")
	}
	clean := strings.TrimPrefix(path.Clean(p), "/")
	if clean == "" || clean == "." {
		return s.sendIndex(c)
	}
	data, err := fs.ReadFile(s.webFS, clean)
	if err != nil {
		return s.sendIndex(c)
	}
	if strings.HasPrefix(clean, "assets/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	ctype := "application/octet-stream"
	if ext := path.Ext(clean); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			ctype = t
		}
	}
	return writeBytes(c, ctype, data)
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

func (s *Server) requireAgent(c *gin.Context) error {
	if s.agentToken == "" {
		return fail(http.StatusServiceUnavailable, "agent token is not configured")
	}
	if !bearerMatch(c.GetHeader("Authorization"), s.agentToken) {
		return fail(http.StatusUnauthorized, "unauthorized")
	}
	return nil
}

func (s *Server) agentAuth(c *gin.Context) {
	if err := s.requireAgent(c); err != nil {
		writeError(c, err)
	}
}

func (s *Server) sendIndex(c *gin.Context) error {
	data, err := fs.ReadFile(s.webFS, "index.html")
	if err != nil {
		return fail(http.StatusInternalServerError, "web UI not built")
	}
	c.Header("Cache-Control", "no-cache")
	return writeBytes(c, "text/html; charset=utf-8", data)
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
	if s.sched != nil {
		s.sched.Sync(clone.Services)
	}
	if s.diag != nil {
		s.diag.Sync(clone.Services)
	}
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
	diag := s.diag
	s.mu.Unlock()

	if sched != nil {
		sched.Sync(cfg.Services)
	}
	if diag != nil {
		diag.Sync(cfg.Services)
	}
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
	timelineCutoff := time.Now().Add(-time.Duration(db.TimelineRetentionDays()) * 24 * time.Hour).Unix()
	if n, err := database.DeleteTimelineBefore(timelineCutoff); err != nil {
		slog.Error("retention: delete timeline", "error", err)
	} else if n > 0 {
		slog.Info("retention: removed old timeline events", "count", n)
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
