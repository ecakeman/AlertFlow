package api

import (
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
)

type deliveryDTO struct {
	ID              int64   `json:"id"`
	EventID         string  `json:"eventId"`
	IncidentID      *int64  `json:"incidentId"`
	IntegrationID   int64   `json:"integrationId"`
	IntegrationName string  `json:"integrationName"`
	IntegrationType string  `json:"integrationType"`
	EventType       string  `json:"eventType"`
	Status          string  `json:"status"`
	AttemptCount    int     `json:"attemptCount"`
	NextAttemptAt   string  `json:"nextAttemptAt"`
	LastError       string  `json:"lastError"`
	SentAt          *string `json:"sentAt"`
}

func toDeliveryDTO(row db.DeliveryView) deliveryDTO {
	return deliveryDTO{
		ID:              row.ID,
		EventID:         row.EventID,
		IncidentID:      row.IncidentID,
		IntegrationID:   row.IntegrationID,
		IntegrationName: row.IntegrationName,
		IntegrationType: row.IntegrationType,
		EventType:       row.EventType,
		Status:          row.Status,
		AttemptCount:    row.AttemptCount,
		NextAttemptAt:   iso(row.NextAttemptAt),
		LastError:       row.LastError,
		SentAt:          isoPtr(row.SentAt),
	}
}

// GET /api/incidents/:id/deliveries → tasks for one incident, without secrets.
func (s *Server) handleListDeliveries(c *gin.Context) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	rows, err := s.conn().ListDeliveriesByIncident(inc.ID)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load deliveries")
	}
	out := make([]deliveryDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDeliveryDTO(row))
	}
	return writeJSON(c, http.StatusOK, out)
}

// POST /api/deliveries/:id/retry → start a new attempt round for a dead task.
func (s *Server) handleRetryDelivery(c *gin.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(http.StatusBadRequest, "invalid delivery id")
	}
	err = s.conn().RetryDeadDelivery(id, time.Now().Unix())
	if errors.Is(err, db.ErrNotFound) {
		return fail(http.StatusNotFound, "delivery not found")
	}
	if errors.Is(err, db.ErrNotRetryable) {
		return fail(http.StatusBadRequest, "only a dead delivery can be retried")
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "could not retry delivery")
	}
	return writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}
