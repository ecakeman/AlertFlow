package api

import (
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/query"
)

// serviceName resolves a service id to its display name, tolerating services
// that were deleted from config.yaml while their incidents remain in PostgreSQL.
func (s *Server) serviceName(id string) string {
	if svc := s.config().Find(id); svc != nil {
		return svc.Name
	}
	return "(deleted service)"
}

// GET /api/incidents?status=&severity=&serviceId=&limit=&offset= → a page of
// incidents (newest first) plus the total matching the filter, for pagination.
// `severity` lets a caller ask for outages only — e.g. a chart's outage bands,
// which shouldn't be crowded out of a limited page by a chatty service's
// warning events.
func (s *Server) handleListIncidents(c *gin.Context) error {
	status := c.Query("status")
	if status != "" && status != "ongoing" && status != "resolved" {
		return fail(http.StatusBadRequest, "status must be ongoing or resolved")
	}
	severity := c.Query("severity")
	if severity != "" && severity != db.SeverityOutage && severity != db.SeverityWarning {
		return fail(http.StatusBadRequest, "severity must be outage or warning")
	}
	serviceID := c.Query("serviceId")

	limit := query.DefaultIncidentPageSize
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	var offset int
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}

	page, err := s.query().Incidents(query.IncidentFilter{
		ServiceID: serviceID, Status: status, Severity: severity,
		Limit: min(limit, query.MaxIncidentPageSize), Offset: offset,
	})
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load incidents")
	}
	out := make([]incidentDTO, 0, len(page.Incidents))
	for _, inc := range page.Incidents {
		out = append(out, toIncidentDTO(inc, s.serviceName(inc.ServiceID)))
	}
	return writeJSON(c, http.StatusOK, incidentsResponse{Incidents: out, Total: page.Total})
}

// GET /api/incidents/:id → an incident with its comments.
func (s *Server) handleGetIncident(c *gin.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(http.StatusBadRequest, "invalid incident id")
	}
	detail, err := s.query().IncidentDetail(id)
	if errors.Is(err, query.ErrIncidentNotFound) {
		return fail(http.StatusNotFound, "incident not found")
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load incident")
	}
	out := incidentDetailDTO{incidentDTO: toIncidentDTO(detail.Incident, s.serviceName(detail.Incident.ServiceID))}
	out.Comments = make([]incidentCommentDTO, 0, len(detail.Comments))
	for _, cm := range detail.Comments {
		out.Comments = append(out.Comments, toIncidentCommentDTO(cm))
	}
	return writeJSON(c, http.StatusOK, out)
}

type incidentInput struct {
	ServiceID  string  `json:"serviceId"`
	Title      string  `json:"title"`
	StartedAt  *string `json:"startedAt"`
	ResolvedAt *string `json:"resolvedAt"`
}

// POST /api/incidents → manually create an incident (admin).
func (s *Server) handleCreateIncident(c *gin.Context) error {
	var in incidentInput
	if err := decode(c, &in); err != nil {
		return err
	}
	svc := s.config().Find(in.ServiceID)
	if svc == nil {
		return fail(http.StatusBadRequest, "unknown service")
	}
	started, err := parseTime(in.StartedAt, time.Now())
	if err != nil {
		return fail(http.StatusBadRequest, "invalid startedAt")
	}
	var title *string
	if t := strings.TrimSpace(in.Title); t != "" {
		title = &t
	}
	var createdBy *int64
	if u := userLocal(c); u != nil {
		createdBy = &u.ID
	}
	var resolvedAt *int64
	if in.ResolvedAt != nil {
		resolved, perr := parseTime(in.ResolvedAt, time.Now())
		if perr != nil {
			return fail(http.StatusBadRequest, "invalid resolvedAt")
		}
		unix := resolved.Unix()
		resolvedAt = &unix
	}
	inc, err := s.conn().CreateManualIncident(db.ManualIncident{
		ServiceID: svc.ID, ServiceName: svc.Name, ServiceURL: svc.URL,
		Title: title, StartedAt: started.Unix(), ResolvedAt: resolvedAt, CreatedBy: createdBy,
	})
	if err != nil {
		if errors.Is(err, db.ErrOngoingExists) {
			return fail(http.StatusBadRequest, err.Error())
		}
		return fail(http.StatusInternalServerError, "could not create incident")
	}
	return writeJSON(c, http.StatusCreated, toIncidentDTO(*inc, s.serviceName(inc.ServiceID)))
}

// PUT /api/incidents/:id → edit title/started/resolved (admin). Providing
// resolvedAt marks it resolved; omitting it keeps the current state.
func (s *Server) handleUpdateIncident(c *gin.Context) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	var in incidentInput
	if err := decode(c, &in); err != nil {
		return err
	}
	title := inc.Title
	if in.Title != "" {
		title = strings.TrimSpace(in.Title)
	}
	started := inc.StartedAt
	if in.StartedAt != nil {
		t, perr := parseTime(in.StartedAt, time.Unix(inc.StartedAt, 0))
		if perr != nil {
			return fail(http.StatusBadRequest, "invalid startedAt")
		}
		started = t.Unix()
	}
	resolved := inc.ResolvedAt
	if in.ResolvedAt != nil {
		t, perr := parseTime(in.ResolvedAt, time.Now())
		if perr != nil {
			return fail(http.StatusBadRequest, "invalid resolvedAt")
		}
		u := t.Unix()
		resolved = &u
	}
	updated, err := s.conn().UpdateIncident(inc.ID, title, started, resolved)
	if err != nil {
		if errors.Is(err, db.ErrOngoingExists) {
			return fail(http.StatusBadRequest, err.Error())
		}
		return fail(http.StatusInternalServerError, "could not update incident")
	}
	return writeJSON(c, http.StatusOK, toIncidentDTO(*updated, s.serviceName(updated.ServiceID)))
}

// DELETE /api/incidents/:id → remove an incident (admin).
func (s *Server) handleDeleteIncident(c *gin.Context) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	if err := s.conn().DeleteIncident(inc.ID); err != nil {
		return fail(http.StatusInternalServerError, "could not delete incident")
	}
	return writeEmpty(c, http.StatusNoContent)
}

// POST /api/incidents/:id/comments → add a comment (any authenticated user).
func (s *Server) handleAddIncidentComment(c *gin.Context) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := decode(c, &in); err != nil {
		return err
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return fail(http.StatusBadRequest, "comment body is required")
	}
	var userID *int64
	if u := userLocal(c); u != nil {
		userID = &u.ID
	}
	cm, err := s.conn().AddIncidentComment(inc.ID, userID, body, time.Now().Unix())
	if err != nil {
		return fail(http.StatusInternalServerError, "could not add comment")
	}
	return writeJSON(c, http.StatusCreated, toIncidentCommentDTO(*cm))
}

// incidentParam parses :id and loads the incident (404 if absent).
func (s *Server) incidentParam(c *gin.Context) (*db.Incident, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return nil, fail(http.StatusBadRequest, "invalid incident id")
	}
	inc, err := s.conn().GetIncident(id)
	if err != nil {
		return nil, fail(http.StatusNotFound, "incident not found")
	}
	return inc, nil
}

// parseTime parses an optional RFC3339 timestamp, falling back to def when nil.
func parseTime(s *string, def time.Time) (time.Time, error) {
	if s == nil || *s == "" {
		return def, nil
	}
	return time.Parse(time.RFC3339, *s)
}
