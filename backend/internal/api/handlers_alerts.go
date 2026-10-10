package api

import (
	"crypto/subtle"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/ecakeman/alertflow/internal/db"
)

const webhookBodyLimit = 64 << 10

type alertWebhookBody struct {
	Source      string `json:"source"`
	EventID     string `json:"event_id"`
	AlertKey    string `json:"alert_key"`
	ServiceID   string `json:"service_id"`
	Status      string `json:"status"`
	OccurredAt  int64  `json:"occurred_at"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// POST /api/alerts/webhook accepts one external alert. A 202 or a duplicate
// 200 means the event is stored. It does not mean an incident or a notification
// has been processed.
func (s *Server) handleAlertWebhook(c fiber.Ctx) error {
	if s.webhookToken == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "webhook token is not configured")
	}
	if !bearerMatch(c.Get("Authorization"), s.webhookToken) {
		return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
	}
	if !s.webhooks.allow(c.IP()) {
		return fiber.NewError(fiber.StatusTooManyRequests, "too many requests")
	}
	if len(c.Body()) > webhookBodyLimit {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body is too large")
	}
	var body alertWebhookBody
	if err := decode(c, &body); err != nil {
		return err
	}
	body.Source = strings.TrimSpace(body.Source)
	body.EventID = strings.TrimSpace(body.EventID)
	body.AlertKey = strings.TrimSpace(body.AlertKey)
	body.ServiceID = strings.TrimSpace(body.ServiceID)
	body.Status = strings.TrimSpace(body.Status)
	body.Title = strings.TrimSpace(body.Title)
	body.Description = strings.TrimSpace(body.Description)
	if reason := validateAlertWebhook(body); reason != "" {
		return fiber.NewError(fiber.StatusBadRequest, reason)
	}
	svc := s.config().Find(body.ServiceID)
	if svc == nil {
		return fiber.NewError(fiber.StatusBadRequest, "unknown service")
	}
	duplicate, err := s.conn().AcceptExternalAlert(db.ExternalAlertIn{
		Source:      body.Source,
		EventID:     body.EventID,
		AlertKey:    body.AlertKey,
		ServiceID:   body.ServiceID,
		ServiceName: svc.Name,
		ServiceURL:  svc.URL,
		Status:      body.Status,
		Title:       body.Title,
		Description: body.Description,
		OccurredAt:  body.OccurredAt,
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not accept alert")
	}
	if duplicate {
		return c.JSON(fiber.Map{"accepted": true, "duplicate": true})
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"accepted": true})
}

func validateAlertWebhook(body alertWebhookBody) string {
	if !shortText(body.Source) || !shortText(body.EventID) || !shortText(body.AlertKey) || !shortText(body.ServiceID) || !shortText(body.Title) {
		return "source, event_id, alert_key, service_id, and title are required"
	}
	if utf8.RuneCountInString(body.Description) > 4000 {
		return "description is too long"
	}
	if body.Status != "firing" && body.Status != "resolved" {
		return "status must be firing or resolved"
	}
	if body.OccurredAt <= 0 {
		return "occurred_at is required"
	}
	return ""
}

func shortText(value string) bool {
	n := utf8.RuneCountInString(value)
	return n > 0 && n <= 512
}

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
