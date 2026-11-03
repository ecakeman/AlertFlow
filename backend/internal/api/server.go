package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/cache"
	"github.com/ecakeman/alertflow/backend/internal/system"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ArtifactSource 由 Python gRPC 实现。为空时产物全文接口返回 503。
type ArtifactSource interface {
	GetArtifact(r *http.Request, episodeID uuid.UUID, artifactType string) (any, error)
}

// Server 是管理与应用上报的 HTTP 入口。
type Server struct {
	Store     *system.Store
	Artifacts ArtifactSource
	Cache     cache.Aside
	Log       *slog.Logger
}

func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// 空列表表示不信任任何代理转发的客户端地址。
	_ = engine.SetTrustedProxies([]string{})
	engine.HandleMethodNotAllowed = true
	engine.Use(s.requestID(), s.accessLog(), s.recover())
	engine.GET("/healthz", func(c *gin.Context) {
		writeJSON(c.Writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	engine.POST("/api/v1/services", s.createService)
	engine.GET("/api/v1/services/:service_id", s.getService)
	engine.PATCH("/api/v1/services/:service_id", s.patchService)
	engine.GET("/api/v1/services/:service_id/timeline", s.serviceTimeline)
	engine.POST("/api/v1/services/:service_id/events", s.postEvent)
	engine.GET("/api/v1/episodes/:episode_id", s.getEpisode)
	engine.GET("/api/v1/episodes/:episode_id/timeline", s.episodeTimeline)
	engine.GET("/api/v1/episodes/:episode_id/artifacts", s.artifacts)
	engine.POST("/api/v1/episodes/:episode_id/retry-diagnosis", s.retryDiagnosis)
	engine.POST("/api/v1/episodes/:episode_id/retry-archive", s.retryArchive)
	return engine
}

func (s *Server) requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func (s *Server) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if s.Log == nil {
			return
		}
		s.Log.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
		)
	}
}

func (s *Server) recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if s.Log != nil {
				s.Log.Error("http panic", "request_id", c.GetString("request_id"), "err", rec)
			}
			if !c.Writer.Written() {
				writeJSON(c.Writer, http.StatusInternalServerError, map[string]string{
					"error_code": "INTERNAL", "message": "internal error",
				})
			}
			c.Abort()
		}()
		c.Next()
	}
}

func (s *Server) createService(c *gin.Context) {
	var in system.CreateServiceInput
	if err := decode(c.Request, &in); err != nil {
		writeErr(c.Writer, err)
		return
	}
	view, err := s.Store.CreateService(c.Request.Context(), in)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusCreated, view)
}

func (s *Server) getService(c *gin.Context) {
	id, err := uuid.Parse(c.Param("service_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid service_id"})
		return
	}
	var view system.ServiceView
	err = s.Cache.Get(c.Request.Context(), "service:"+id.String(), &view, func() (any, error) {
		return s.Store.GetService(c.Request.Context(), id)
	})
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, view)
}

func (s *Server) patchService(c *gin.Context) {
	id, err := uuid.Parse(c.Param("service_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid service_id"})
		return
	}
	var in system.PatchServiceInput
	if err := decode(c.Request, &in); err != nil {
		writeErr(c.Writer, err)
		return
	}
	view, err := s.Store.PatchService(c.Request.Context(), id, in)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, view)
}

func (s *Server) serviceTimeline(c *gin.Context) {
	id, err := uuid.Parse(c.Param("service_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid service_id"})
		return
	}
	records, err := s.Store.ServiceTimeline(c.Request.Context(), id, 100)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	if records == nil {
		records = []system.TimelineRecord{}
	}
	writeJSON(c.Writer, http.StatusOK, map[string]any{"records": records})
}

func (s *Server) postEvent(c *gin.Context) {
	id, err := uuid.Parse(c.Param("service_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid service_id"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "cannot read body"})
		return
	}
	var ev system.AppEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid json"})
		return
	}
	res, err := s.Store.ApplyAppEvent(c.Request.Context(), id, ev, raw)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, res.Status, res.Body)
}

func (s *Server) getEpisode(c *gin.Context) {
	id, err := uuid.Parse(c.Param("episode_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid episode_id"})
		return
	}
	view, err := s.Store.GetEpisode(c.Request.Context(), id)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, view)
}

func (s *Server) episodeTimeline(c *gin.Context) {
	id, err := uuid.Parse(c.Param("episode_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid episode_id"})
		return
	}
	body, err := s.Store.EpisodeTimeline(c.Request.Context(), id)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, body)
}

func (s *Server) artifacts(c *gin.Context) {
	if s.Artifacts == nil {
		writeJSON(c.Writer, http.StatusServiceUnavailable, map[string]string{"error_code": "AGENT_UNAVAILABLE", "message": "artifact service is not configured"})
		return
	}
	id, err := uuid.Parse(c.Param("episode_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid episode_id"})
		return
	}
	body, err := s.Artifacts.GetArtifact(c.Request, id, c.Query("type"))
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, body)
}

func (s *Server) retryDiagnosis(c *gin.Context) {
	id, err := uuid.Parse(c.Param("episode_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid episode_id"})
		return
	}
	taskID, err := s.Store.RetryDiagnosis(c.Request.Context(), id)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, map[string]any{"task_id": taskID})
}

func (s *Server) retryArchive(c *gin.Context) {
	id, err := uuid.Parse(c.Param("episode_id"))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid episode_id"})
		return
	}
	taskID, err := s.Store.RetryArchive(c.Request.Context(), id)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, map[string]any{"task_id": taskID})
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &system.Error{Code: "VALIDATION", Message: err.Error(), HTTP: 400}
	}
	return nil
}

func writeErr(w http.ResponseWriter, err error) {
	var se *system.Error
	if errors.As(err, &se) {
		writeJSON(w, se.HTTP, map[string]string{"error_code": se.Code, "message": se.Message})
		return
	}
	writeJSON(w, 500, map[string]string{"error_code": "INTERNAL", "message": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func artifactType(raw string) string {
	switch strings.ToUpper(raw) {
	case "DIAGNOSIS_REPORT":
		return "DIAGNOSIS_REPORT"
	default:
		return "INVESTIGATION"
	}
}
