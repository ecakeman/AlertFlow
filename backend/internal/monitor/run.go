package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ecakeman/alertflow/internal/cache"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/diagnostics"
	"github.com/ecakeman/alertflow/internal/observe/kubernetes"
	"github.com/ecakeman/alertflow/internal/query"
)

// Loop runs the per-service check and diagnostics workers for one config
// directory. It does not listen for HTTP. A failed reload keeps the last
// snapshot that loaded.
type Loop struct {
	DB    *db.DB
	Dir   string
	Cache *cache.Store
	// Every is how often the directory is re-read. Zero means one second.
	Every time.Duration

	mu      sync.RWMutex
	current *config.Config
}

// Run syncs workers from disk until ctx is cancelled, then waits for them to
// leave their current cycle.
func (l *Loop) Run(ctx context.Context) error {
	every := l.Every
	if every <= 0 {
		every = time.Second
	}
	sched := New(l.DB)
	sched.Configured = l.present
	if l.Cache != nil && l.Cache.Enabled() {
		reads := query.New(l.DB, l.snapshot)
		reads.Cache = l.Cache
		sched.OnCheckStored = func(serviceID string) {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			reads.InvalidateService(cctx, serviceID)
		}
	}
	diag := diagnostics.New(l.DB)
	diag.Configured = l.present
	cluster := kubernetes.New(l.DB, l.present)
	if kubernetes.Enabled() {
		if err := cluster.Connect(); err != nil {
			slog.Error("kubernetes: connection failed; checks continue", "error", err)
		}
	}
	defer sched.Stop()
	defer diag.Stop()
	defer cluster.Close()

	l.apply(sched, diag, cluster)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.apply(sched, diag, cluster)
		}
	}
}

func (l *Loop) apply(sched *Scheduler, diag *diagnostics.Collector, cluster *kubernetes.Observer) {
	loaded, err := config.Load(l.Dir)
	if err != nil {
		slog.Warn("monitor: config reload failed", "error", err)
		l.mu.RLock()
		loaded = l.current
		l.mu.RUnlock()
		if loaded == nil {
			return
		}
	} else {
		l.mu.Lock()
		l.current = loaded
		l.mu.Unlock()
	}
	sched.Sync(loaded.Services)
	diag.Sync(loaded.Services)
	cluster.SyncTargets(loaded.Services, loaded.Settings.Kubernetes)
}

func (l *Loop) present(serviceID string) bool {
	l.mu.RLock()
	fallback := l.current
	l.mu.RUnlock()
	return config.ServiceConfigured(l.Dir, serviceID, fallback)
}

func (l *Loop) snapshot() *config.Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.current == nil {
		return &config.Config{}
	}
	return l.current
}
