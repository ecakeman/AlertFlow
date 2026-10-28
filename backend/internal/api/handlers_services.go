package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/image"
	"github.com/ecakeman/alertflow/internal/query"
)

var errServiceNotFound = errors.New("service not found")

// GET /api/services → all services with current status + metrics.
func (s *Server) handleListServices(c *gin.Context) error {
	list, err := s.query().ServiceList()
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load metrics")
	}
	out := make([]serviceDTO, 0, len(list))
	for _, item := range list {
		out = append(out, toServiceDTO(item.Service, item.Metrics))
	}
	return writeJSON(c, http.StatusOK, out)
}

type serviceInput struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Interval int    `json:"interval"`
	Mode     string `json:"mode"`
	// RetryAttempts 0 and a nil RetryDelays mean "leave alone" on update and
	// "inherit the global default" on create, matching Interval's convention.
	RetryAttempts int   `json:"retryAttempts"`
	RetryDelays   []int `json:"retryDelays"`
}

// POST /api/services → add a service (persisted to config.yaml).
func (s *Server) handleCreateService(c *gin.Context) error {
	var in serviceInput
	if err := decode(c, &in); err != nil {
		return err
	}
	id := config.Slugify(in.Name)
	if id == "" {
		return fail(http.StatusBadRequest, "a valid name is required")
	}
	err := s.updateConfig(func(cfg *config.Config) error {
		if cfg.Find(id) != nil {
			return fmt.Errorf("a service named %q already exists", in.Name)
		}
		mode := in.Mode
		if mode == "" {
			mode = cfg.Settings.DefaultWidgetMode
		}
		w, h := defaultSize(mode)
		interval := in.Interval
		if interval == 0 {
			interval = cfg.Settings.Check.DefaultInterval
		}
		cfg.Services = append(cfg.Services, config.Service{
			ID:   id,
			Name: in.Name,
			URL:  in.URL,
			Check: config.ServiceCheck{
				Interval: interval, Method: "GET", Timeout: cfg.Settings.Check.Timeout,
				RetryAttempts: in.RetryAttempts,
				RetryDelays:   append([]int(nil), in.RetryDelays...),
			},
			Widget: config.Widget{Mode: mode},
			Layout: config.Layout{X: 0, Y: maxBottom(cfg.Services), W: w, H: h},
		})
		return nil
	})
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	return writeJSON(c, http.StatusCreated, toServiceDTO(*s.config().Find(id), nil))
}

// PUT /api/services/:id → edit a service.
func (s *Server) handleUpdateService(c *gin.Context) error {
	id := c.Param("id")
	var in serviceInput
	if err := decode(c, &in); err != nil {
		return err
	}
	err := s.updateConfig(func(cfg *config.Config) error {
		svc := cfg.Find(id)
		if svc == nil {
			return errServiceNotFound
		}
		if in.Name != "" {
			svc.Name = in.Name
		}
		if in.URL != "" {
			svc.URL = in.URL
		}
		if in.Interval != 0 {
			svc.Check.Interval = in.Interval
		}
		if in.Mode != "" {
			svc.Widget.Mode = in.Mode
		}
		if in.RetryAttempts != 0 {
			svc.Check.RetryAttempts = in.RetryAttempts
		}
		// Copy: the decoded request slice must not end up aliased into the
		// live config once the clone is swapped in.
		if in.RetryDelays != nil {
			svc.Check.RetryDelays = append([]int(nil), in.RetryDelays...)
		}
		return nil
	})
	if err != nil {
		return configErr(err)
	}
	status, err := s.query().ServiceStatusFor(id)
	if err != nil {
		return configErr(err)
	}
	return writeJSON(c, http.StatusOK, toServiceDTO(status.Service, status.Metrics))
}

// DELETE /api/services/:id → stop checks, remove runtime rows, then drop the
// service from config. Config stays when runtime cleanup fails.
func (s *Server) handleDeleteService(c *gin.Context) error {
	id := c.Param("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Find(id) == nil {
		return configErr(errServiceNotFound)
	}
	if s.sched != nil {
		s.sched.Release(id)
	}
	if err := s.database.DeleteServiceRecords(id); err != nil {
		slog.Error("delete service history", "service", id, "error", err)
		if s.sched != nil {
			s.sched.Sync(s.cfg.Services)
		}
		return fail(http.StatusInternalServerError, "could not delete service history")
	}
	if err := image.Delete(config.ImagesPath(s.configDir), id); err != nil {
		slog.Error("delete service image", "service", id, "error", err)
		if s.sched != nil {
			s.sched.Sync(s.cfg.Services)
		}
		return fail(http.StatusInternalServerError, "could not delete service image")
	}
	next := s.cfg.Clone()
	kept := make([]config.Service, 0, len(next.Services))
	for _, svc := range next.Services {
		if svc.ID != id {
			kept = append(kept, svc)
		}
	}
	next.Services = kept
	if err := config.Save(s.configDir, next); err != nil {
		slog.Error("delete service config", "service", id, "error", err)
		if s.sched != nil {
			s.sched.Sync(s.cfg.Services)
		}
		return fail(http.StatusInternalServerError, "could not delete service")
	}
	s.cfg = next
	if s.sched != nil {
		s.sched.Sync(next.Services)
	}
	return writeEmpty(c, http.StatusNoContent)
}

type layoutItem struct {
	ID    string `json:"id"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Mode  string `json:"mode"`
	Chart string `json:"chart"`
}

// PATCH /api/services/layout → bulk-save grid positions, widget modes and chart
// styles. Mode and chart are only applied when set, so a plain layout save (the
// common case, on every drag) does not reset them.
func (s *Server) handleUpdateLayout(c *gin.Context) error {
	var items []layoutItem
	if err := decode(c, &items); err != nil {
		return err
	}
	err := s.updateConfig(func(cfg *config.Config) error {
		for _, it := range items {
			svc := cfg.Find(it.ID)
			if svc == nil {
				continue
			}
			svc.Layout = config.Layout{X: it.X, Y: it.Y, W: it.W, H: it.H}
			if it.Mode != "" {
				svc.Widget.Mode = it.Mode
			}
			if it.Chart != "" {
				svc.Chart.Type = it.Chart
			}
		}
		return nil
	})
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	return writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/services/:id/check → run a check immediately.
func (s *Server) handleCheckNow(c *gin.Context) error {
	id := c.Param("id")
	svc := s.config().Find(id)
	if svc == nil {
		return fail(http.StatusNotFound, "service not found")
	}
	s.scheduler().CheckNow(*svc)
	status, err := s.query().ServiceStatusFor(id)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load metrics")
	}
	return writeJSON(c, http.StatusOK, toServiceDTO(status.Service, status.Metrics))
}

type metricsResponse struct {
	serviceDTO
	Series        []db.SeriesPoint `json:"series"`
	From          int64            `json:"from"`
	To            int64            `json:"to"`
	BucketSeconds int64            `json:"bucketSeconds"`
	UptimeWindows uptimeWindowsDTO `json:"uptimeWindows"`
	TLS           *tlsDTO          `json:"tls"`
}

// GET /api/services/:id/metrics?range=1h|6h|24h|7d|30d|365d → aggregates, time
// series, multi-window uptime and current TLS certificate info.
func (s *Server) handleServiceMetrics(c *gin.Context) error {
	id := c.Param("id")
	res, err := s.query().ServiceMetrics(c.Request.Context(), id, c.Query("range"), time.Now())
	if errors.Is(err, query.ErrServiceNotFound) {
		return fail(http.StatusNotFound, "service not found")
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load metrics")
	}
	return writeJSON(c, http.StatusOK, metricsResponse{
		serviceDTO:    toServiceDTO(res.Service, res.Status),
		Series:        res.Series,
		From:          res.From,
		To:            res.To,
		BucketSeconds: res.BucketSeconds,
		UptimeWindows: uptimeWindowsDTO{Days7: res.Uptime.Days7, Days30: res.Uptime.Days30, Days365: res.Uptime.Days365},
		TLS:           toTLSDTO(res.TLS),
	})
}

// GET /api/services/:id/checks?limit=&before= → stored check cycles, newest
// first, for the service's ping console. Rows come back un-squashed: collapsing
// runs of successes is a rendering concern, and doing it here would break
// across page boundaries.
func (s *Server) handleServiceChecks(c *gin.Context) error {
	id := c.Param("id")
	limit := query.DefaultCheckPageSize
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	var before int64
	if v, err := strconv.ParseInt(c.Query("before"), 10, 64); err == nil && v > 0 {
		before = v
	}
	page, err := s.query().CheckHistory(c.Request.Context(), id, before, limit)
	if errors.Is(err, query.ErrServiceNotFound) {
		return fail(http.StatusNotFound, "service not found")
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load checks")
	}
	out := make([]checkRowDTO, 0, len(page.Checks))
	for _, r := range page.Checks {
		out = append(out, toCheckRowDTO(r))
	}
	return writeJSON(c, http.StatusOK, checksResponse{Checks: out, NextBefore: page.NextBefore})
}

// configErr maps a config-update error to a 404 or 400 HTTP error.
func configErr(err error) error {
	if errors.Is(err, errServiceNotFound) {
		return fail(http.StatusNotFound, "service not found")
	}
	return fail(http.StatusBadRequest, err.Error())
}

func defaultSize(mode string) (int, int) {
	if mode == config.ModeDashboard {
		return 3, 4
	}
	return 2, 2
}

func maxBottom(services []config.Service) int {
	y := 0
	for _, svc := range services {
		if b := svc.Layout.Y + svc.Layout.H; b > y {
			y = b
		}
	}
	return y
}
