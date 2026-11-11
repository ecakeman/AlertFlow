package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// readyz 检查数据库可用性，用于就绪探针。
func (s *Server) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	var one int
	if err := s.Store.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		writeJSON(c.Writer, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "database": "down",
		})
		return
	}
	writeJSON(c.Writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) dashboard(c *gin.Context) {
	summary, err := s.Store.Dashboard(c.Request.Context())
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	writeJSON(c.Writer, http.StatusOK, summary)
}

func (s *Server) listServices(c *gin.Context) {
	limit, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	offset, ok := queryInt(c, "offset")
	if !ok {
		return
	}
	services, total, err := s.Store.ListServices(c.Request.Context(), system.ServiceFilter{
		Status: c.Query("status"),
		Q:      c.Query("q"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	if services == nil {
		services = []system.ServiceSummary{}
	}
	writeJSON(c.Writer, http.StatusOK, map[string]any{
		"services": services, "total": total, "limit": pageLimit(limit, 50, 200), "offset": offset,
	})
}

func (s *Server) deleteService(c *gin.Context) {
	id, ok := parseUUIDParam(c, "service_id", "invalid service_id")
	if !ok {
		return
	}
	if err := s.Store.SoftDeleteService(c.Request.Context(), id); err != nil {
		writeErr(c.Writer, err)
		return
	}
	s.Cache.Delete(c.Request.Context(), "service:"+id.String())
	c.Status(http.StatusNoContent)
}

func (s *Server) listTargets(c *gin.Context) {
	id, ok := parseUUIDParam(c, "service_id", "invalid service_id")
	if !ok {
		return
	}
	targets, err := s.Store.GetTargets(c.Request.Context(), id)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	if targets == nil {
		targets = []system.Target{}
	}
	writeJSON(c.Writer, http.StatusOK, map[string]any{"targets": targets})
}

func (s *Server) createTarget(c *gin.Context) {
	id, ok := parseUUIDParam(c, "service_id", "invalid service_id")
	if !ok {
		return
	}
	var in system.TargetInput
	if err := decode(c.Request, &in); err != nil {
		writeErr(c.Writer, err)
		return
	}
	target, err := s.Store.CreateTarget(c.Request.Context(), id, in)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	s.Cache.Delete(c.Request.Context(), "service:"+id.String())
	writeJSON(c.Writer, http.StatusCreated, target)
}

func (s *Server) patchTarget(c *gin.Context) {
	id, ok := parseUUIDParam(c, "target_id", "invalid target_id")
	if !ok {
		return
	}
	var in system.PatchTargetInput
	if err := decode(c.Request, &in); err != nil {
		writeErr(c.Writer, err)
		return
	}
	target, serviceID, err := s.Store.UpdateTarget(c.Request.Context(), id, in)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	s.Cache.Delete(c.Request.Context(), "service:"+serviceID.String())
	writeJSON(c.Writer, http.StatusOK, target)
}

func (s *Server) deleteTarget(c *gin.Context) {
	id, ok := parseUUIDParam(c, "target_id", "invalid target_id")
	if !ok {
		return
	}
	serviceID, err := s.Store.SoftDeleteTarget(c.Request.Context(), id)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	s.Cache.Delete(c.Request.Context(), "service:"+serviceID.String())
	c.Status(http.StatusNoContent)
}

func (s *Server) listEpisodes(c *gin.Context) {
	limit, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	offset, ok := queryInt(c, "offset")
	if !ok {
		return
	}
	filter := system.EpisodeFilter{Status: c.Query("status"), Limit: limit, Offset: offset}
	if raw := c.Query("service_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid service_id"})
			return
		}
		filter.ServiceID = &id
	}
	if raw := c.Query("from"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid from"})
			return
		}
		filter.From = &ts
	}
	if raw := c.Query("to"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid to"})
			return
		}
		filter.To = &ts
	}
	episodes, total, err := s.Store.ListEpisodes(c.Request.Context(), filter)
	if err != nil {
		writeErr(c.Writer, err)
		return
	}
	if episodes == nil {
		episodes = []system.EpisodeSummary{}
	}
	writeJSON(c.Writer, http.StatusOK, map[string]any{
		"episodes": episodes, "total": total, "limit": pageLimit(limit, 20, 100), "offset": offset,
	})
}

// parseUUIDParam 解析路径中的 UUID，失败时写 400 并返回 false。
func parseUUIDParam(c *gin.Context, name, message string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": message})
		return uuid.Nil, false
	}
	return id, true
}

// queryInt 解析可选整数查询参数，失败时写 400 并返回 false。
func queryInt(c *gin.Context, name string) (int, bool) {
	raw := c.Query(name)
	if raw == "" {
		return 0, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeJSON(c.Writer, 400, map[string]string{"error_code": "VALIDATION", "message": "invalid " + name})
		return 0, false
	}
	return v, true
}

// pageLimit 与 Store 的默认值/上限保持一致用于响应。
func pageLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
