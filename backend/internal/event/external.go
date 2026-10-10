package event

import "fmt"

const (
	// TypeExternalAlert is an alert accepted from the generic JSON webhook.
	// It is not a check result and must not be stored as online or offline.
	TypeExternalAlert = "external.alert"
)

// ExternalAlert is one accepted webhook event. EventID is the outbox id.
// CallerEventID is the id the caller sent, unique only together with Source.
type ExternalAlert struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	SchemaVersion int    `json:"schema_version"`
	OccurredAt    int64  `json:"occurred_at"`
	Source        string `json:"source"`
	CallerEventID string `json:"caller_event_id"`
	AlertKey      string `json:"alert_key"`
	ServiceID     string `json:"service_id"`
	ServiceName   string `json:"service_name"`
	ServiceURL    string `json:"service_url"`
	Status        string `json:"status"`
	Title         string `json:"title"`
	Description   string `json:"description,omitempty"`
}

// ExternalOutboxID is stable for one source and caller event id.
// The length prefix keeps two different pairs from sharing one outbox id.
func ExternalOutboxID(source, callerEventID string) string {
	return fmt.Sprintf("external:%d:%s:%s", len(source), source, callerEventID)
}
