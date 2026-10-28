// Package event holds the versioned payloads written to the outbox.
// Decide says what an event does to incidents. It does not write to the database.
package event

import (
	"crypto/rand"
	"fmt"
)

const (
	// TypeCheckCompleted is the only event produced by a finished check cycle.
	TypeCheckCompleted = "monitor.check.completed"
	// SchemaVersion is the payload version writers and future consumers share.
	SchemaVersion = 1
)

// CheckCompleted is one finished check cycle. Service name and URL are copied
// at record time so a later config edit cannot change what this result meant.
// EventID is assigned once and is not regenerated when the row is retried.
type CheckCompleted struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	SchemaVersion int    `json:"schema_version"`
	OccurredAt    int64  `json:"occurred_at"`
	// Epoch is the service life this check belongs to. Nil means the payload
	// was written before epochs existed; a pointer to 0 is the first life.
	Epoch       *int64 `json:"epoch,omitempty"`
	ServiceID   string `json:"service_id"`
	ServiceName string `json:"service_name"`
	ServiceURL  string `json:"service_url"`
	CheckID     int64  `json:"check_id"`
	Status      string `json:"status"`
	LatencyMs   *int   `json:"latency_ms"`
	StatusCode  *int   `json:"status_code"`
	Error       string `json:"error"`
	Attempts    int    `json:"attempts"`
}

// NewCheckCompleted builds a version-1 event. CheckID is filled after the
// check row insert, inside the same database transaction.
func NewCheckCompleted(serviceID, serviceName, serviceURL string, occurredAt int64, status string, latency, code *int, errMsg string, attempts int) (CheckCompleted, error) {
	id, err := newEventID()
	if err != nil {
		return CheckCompleted{}, err
	}
	if attempts < 1 {
		attempts = 1
	}
	return CheckCompleted{
		EventID:       id,
		EventType:     TypeCheckCompleted,
		SchemaVersion: SchemaVersion,
		OccurredAt:    occurredAt,
		ServiceID:     serviceID,
		ServiceName:   serviceName,
		ServiceURL:    serviceURL,
		CheckID:       0,
		Status:        status,
		LatencyMs:     latency,
		StatusCode:    code,
		Error:         errMsg,
		Attempts:      attempts,
	}, nil
}

func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("event id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
