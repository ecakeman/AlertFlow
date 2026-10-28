// Package diagnostics polls optional per-service application snapshots and
// records them on the runtime timeline. It does not open incidents.
package diagnostics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

const maxBody = 512 * 1024

// Collector runs one loop per service that has a diagnostics URL.
type Collector struct {
	db     *db.DB
	client *http.Client

	// lifecycle serialises Sync, Release and Stop so a cancelled loop is waited
	// on before another one is started or the call returns.
	lifecycle sync.Mutex
	mu        sync.Mutex
	workers   map[string]*worker

	// beforeStore, when set, runs after a snapshot is fetched and before it is
	// written. Tests use it to hold a poll open.
	beforeStore func()
}

type worker struct {
	mu     sync.Mutex
	svc    config.Service
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *worker) service() config.Service {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.svc
}

// New starts no loops until Sync.
func New(database *db.DB) *Collector {
	return &Collector{
		db: database,
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		workers: map[string]*worker{},
	}
}

// Sync keeps one worker for each service that has a diagnostics URL. A replaced
// or removed loop is cancelled and waited on before this call returns, and a
// replacement does not start until that wait finishes.
func (c *Collector) Sync(services []config.Service) {
	if c == nil {
		return
	}
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	wanted := map[string]bool{}
	var restart []config.Service
	var wait []*worker
	for _, svc := range services {
		if strings.TrimSpace(svc.Diagnostics.URL) == "" {
			continue
		}
		wanted[svc.ID] = true
		existing := c.workers[svc.ID]
		if existing != nil {
			existing.mu.Lock()
			same := existing.svc.Diagnostics == svc.Diagnostics
			if same {
				existing.svc = svc
			}
			existing.mu.Unlock()
			if same {
				continue
			}
			existing.cancel()
			delete(c.workers, svc.ID)
			wait = append(wait, existing)
			restart = append(restart, svc)
			continue
		}
		c.start(svc)
	}
	for id, w := range c.workers {
		if !wanted[id] {
			w.cancel()
			delete(c.workers, id)
			wait = append(wait, w)
		}
	}
	c.mu.Unlock()
	for _, w := range wait {
		<-w.done
	}
	if len(restart) == 0 {
		return
	}
	c.mu.Lock()
	for _, svc := range restart {
		if c.workers[svc.ID] == nil {
			c.start(svc)
		}
	}
	c.mu.Unlock()
}

func (c *Collector) start(svc config.Service) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{svc: svc, cancel: cancel, done: make(chan struct{})}
	c.workers[svc.ID] = w
	go func() {
		defer close(w.done)
		c.run(ctx, w)
	}()
}

func (c *Collector) run(ctx context.Context, w *worker) {
	c.poll(ctx, w.service())
	interval := time.Duration(intervalSeconds(w.service())) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx, w.service())
		}
	}
}

// Release stops one service and waits until its current request finishes.
func (c *Collector) Release(id string) {
	if c == nil {
		return
	}
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	w := c.workers[id]
	if w != nil {
		delete(c.workers, id)
		w.cancel()
	}
	c.mu.Unlock()
	if w != nil {
		<-w.done
	}
}

// Stop cancels every worker and waits until each has left its current poll.
func (c *Collector) Stop() {
	if c == nil {
		return
	}
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	workers := c.workers
	c.workers = map[string]*worker{}
	for _, w := range workers {
		w.cancel()
	}
	c.mu.Unlock()
	for _, w := range workers {
		<-w.done
	}
}

// Poll fetches one snapshot and stores it. Tests call it directly.
func (c *Collector) Poll(ctx context.Context, svc config.Service) error {
	if strings.TrimSpace(svc.Diagnostics.URL) == "" {
		return nil
	}
	last, err := c.db.LastDiagnosticSuccess(svc.ID)
	if err != nil {
		return err
	}
	until := time.Now().UTC()
	since := until.Add(-time.Duration(intervalSeconds(svc)) * time.Second)
	if last > 0 {
		since = time.Unix(last, 0).UTC()
	}
	pull, err := c.fetch(ctx, svc, since, until)
	if err != nil {
		return err
	}
	if c.beforeStore != nil {
		c.beforeStore()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.db.ApplyDiagnostics(pull)
}

func (c *Collector) poll(ctx context.Context, svc config.Service) {
	if ctx.Err() != nil {
		return
	}
	if err := c.Poll(ctx, svc); err != nil && ctx.Err() == nil {
		slog.Error("diagnostics: poll", "service", svc.ID, "error", err)
	}
}

func (c *Collector) fetch(ctx context.Context, svc config.Service, since, until time.Time) (db.DiagnosticPull, error) {
	pull := db.DiagnosticPull{ServiceID: svc.ID, ObservedAt: until.Unix()}
	endpoint, err := url.Parse(svc.Diagnostics.URL)
	if err != nil {
		pull.Status = "invalid"
		pull.Error = "invalid diagnostics url"
		return pull, nil
	}
	query := endpoint.Query()
	query.Set("since", since.Format(time.RFC3339))
	query.Set("until", until.Format(time.RFC3339))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return db.DiagnosticPull{}, err
	}
	if name := svc.Diagnostics.TokenEnv; name != "" {
		if token := os.Getenv(name); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return db.DiagnosticPull{}, ctx.Err()
		}
		pull.Status = "unavailable"
		pull.Error = "request failed"
		return pull, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		pull.Status = "invalid"
		pull.Error = "redirect"
		return pull, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		pull.Status = "unavailable"
		pull.Error = "unexpected status"
		return pull, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		pull.Status = "unavailable"
		pull.Error = "read failed"
		return pull, nil
	}
	if len(body) > maxBody {
		pull.Status = "invalid"
		pull.Error = "response too large"
		return pull, nil
	}
	snap := ParseSnapshot(body, until)
	pull.Status = snap.Coverage
	if snap.Coverage == "invalid" {
		pull.Error = "invalid snapshot"
		return pull, nil
	}
	pull.Success = true
	for _, item := range snap.Items {
		pull.Items = append(pull.Items, db.DiagnosticItem{
			Kind: item.Kind, Name: item.Name, Status: item.Status, Summary: item.Summary,
			ObservedAt: item.ObservedAt.Unix(),
		})
	}
	return pull, nil
}

func intervalSeconds(svc config.Service) int {
	sec := svc.Diagnostics.Interval
	if sec == 0 {
		sec = config.DefaultDiagnosticsInterval
	}
	if sec < 15 {
		sec = 15
	}
	return sec
}
