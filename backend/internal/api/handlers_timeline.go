package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/query"
)

const timelineDetailsLimit = 4096

type timelineResourceDTO struct {
	ClusterID string `json:"clusterId,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

type timelineEventDTO struct {
	ID         int64                `json:"id"`
	EventKey   string               `json:"eventKey"`
	ServiceID  string               `json:"serviceId"`
	Source     string               `json:"source"`
	EventType  string               `json:"eventType"`
	Severity   string               `json:"severity"`
	Title      string               `json:"title"`
	Summary    string               `json:"summary"`
	OccurredAt string               `json:"occurredAt"`
	ObservedAt string               `json:"observedAt"`
	RecordedAt string               `json:"recordedAt"`
	Resource   *timelineResourceDTO `json:"resource,omitempty"`
	IncidentID *int64               `json:"incidentId,omitempty"`
	Details    json.RawMessage      `json:"details"`
}

type timelineCoverageDTO struct {
	Status        string  `json:"status"`
	LastSuccessAt *string `json:"lastSuccessAt"`
	LastError     string  `json:"lastError,omitempty"`
}

type timelineWindowDTO struct {
	From          string `json:"from"`
	To            string `json:"to"`
	RetentionDays int    `json:"retentionDays"`
}

type timelineResponse struct {
	Events     []timelineEventDTO             `json:"events"`
	NextCursor *string                        `json:"nextCursor"`
	Window     timelineWindowDTO              `json:"window"`
	Coverage   map[string]timelineCoverageDTO `json:"coverage"`
}

// GET /api/services/:id/timeline?from=&to=&sources=&types=&limit=&cursor=
func (s *Server) handleServiceTimeline(c *gin.Context) error {
	req, err := timelineRequest(c)
	if err != nil {
		return err
	}
	page, err := s.query().ServiceTimeline(c.Param("id"), req)
	if errors.Is(err, query.ErrServiceNotFound) {
		return fail(http.StatusNotFound, "service not found")
	}
	if errors.Is(err, query.ErrTimelineRange) || errors.Is(err, query.ErrTimelineFilter) {
		return fail(http.StatusBadRequest, err.Error())
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load timeline")
	}
	events := make([]timelineEventDTO, 0, len(page.Events))
	for _, ev := range page.Events {
		events = append(events, toTimelineEventDTO(ev))
	}
	var next *string
	if page.HasNext {
		encoded := encodeTimelineCursor(page.NextCursorAt, page.NextCursorID)
		next = &encoded
	}
	coverage := make(map[string]timelineCoverageDTO, len(page.Coverage))
	for name, item := range page.Coverage {
		dto := timelineCoverageDTO{Status: item.Status, LastError: item.LastError}
		if item.LastSuccess != nil {
			text := iso(*item.LastSuccess)
			dto.LastSuccessAt = &text
		}
		coverage[name] = dto
	}
	return writeJSON(c, http.StatusOK, timelineResponse{
		Events:     events,
		NextCursor: next,
		Window: timelineWindowDTO{
			From:          page.From.Format(time.RFC3339),
			To:            page.To.Format(time.RFC3339),
			RetentionDays: page.RetentionDays,
		},
		Coverage: coverage,
	})
}

func timelineRequest(c *gin.Context) (query.TimelineRequest, error) {
	var req query.TimelineRequest
	var err error
	if raw := c.Query("from"); raw != "" {
		req.From, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return req, fail(http.StatusBadRequest, "from must be RFC3339")
		}
	}
	if raw := c.Query("to"); raw != "" {
		req.To, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return req, fail(http.StatusBadRequest, "to must be RFC3339")
		}
	}
	if raw := strings.TrimSpace(c.Query("sources")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !query.TimelineSourceAllowed(part) {
				return req, fail(http.StatusBadRequest, "unknown timeline source")
			}
			req.Sources = append(req.Sources, part)
		}
	}
	if raw := strings.TrimSpace(c.Query("types")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			req.Types = append(req.Types, part)
		}
		if len(req.Types) > 10 {
			return req, fail(http.StatusBadRequest, "too many timeline types")
		}
	}
	if raw := c.Query("limit"); raw != "" {
		req.Limit, err = strconv.Atoi(raw)
		if err != nil || req.Limit < 1 {
			return req, fail(http.StatusBadRequest, "limit must be a positive integer")
		}
	}
	if raw := c.Query("cursor"); raw != "" {
		req.CursorAt, req.CursorID, err = decodeTimelineCursor(raw)
		if err != nil {
			return req, fail(http.StatusBadRequest, "cursor is invalid")
		}
		req.HasCursor = true
	}
	req.Now = time.Now()
	return req, nil
}

func toTimelineEventDTO(ev db.RuntimeEvent) timelineEventDTO {
	dto := timelineEventDTO{
		ID: ev.ID, EventKey: ev.EventKey, ServiceID: ev.ServiceID, Source: ev.Source,
		EventType: ev.EventType, Severity: ev.Severity, Title: ev.Title, Summary: ev.Summary,
		OccurredAt: iso(ev.OccurredAt), ObservedAt: iso(ev.ObservedAt), RecordedAt: iso(ev.RecordedAt),
		Details: capDetails(ev.Details),
	}
	if ev.IncidentID.Valid {
		id := ev.IncidentID.Int64
		dto.IncidentID = &id
	}
	res := timelineResourceDTO{
		ClusterID: ev.ClusterID.String, Namespace: ev.Namespace.String,
		Kind: ev.ResourceKind.String, Name: ev.ResourceName.String, UID: ev.ResourceUID.String,
	}
	if res != (timelineResourceDTO{}) {
		dto.Resource = &res
	}
	return dto
}

func capDetails(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`)
	}
	if len(raw) > timelineDetailsLimit {
		return json.RawMessage(`{"truncated":true}`)
	}
	return raw
}

func encodeTimelineCursor(occurredAt, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(occurredAt, 10) + ":" + strconv.FormatInt(id, 10)))
}

func decodeTimelineCursor(raw string) (int64, int64, error) {
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, 0, err
	}
	occurred, id, ok := strings.Cut(string(buf), ":")
	if !ok {
		return 0, 0, errors.New("cursor")
	}
	at, err := strconv.ParseInt(occurred, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || rowID < 1 || at < 1 {
		return 0, 0, errors.New("cursor")
	}
	return at, rowID, nil
}
