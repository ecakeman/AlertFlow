package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const DiagnosticSource = "application_diagnostics"

// DiagnosticItem is one accepted entry from an application snapshot.
type DiagnosticItem struct {
	Kind       string
	Name       string
	Status     string
	Summary    string
	ObservedAt int64
	EventID    string
	Attributes map[string]string
	Detail     json.RawMessage
}

// DiagnosticPull is one fetch of a service's diagnostics snapshot.
type DiagnosticPull struct {
	ServiceID  string
	Status     string
	Error      string
	ObservedAt int64
	Items      []DiagnosticItem
	// Success is false for unavailable and invalid pulls. Those do not move
	// the cursor or write timeline events.
	Success bool
}

// LastDiagnosticSuccess is the previous successful pull, or 0 when there has
// not been one.
func (db *DB) LastDiagnosticSuccess(serviceID string) (int64, error) {
	var at sql.NullInt64
	err := db.QueryRow(
		`SELECT last_success_at FROM runtime_source_state WHERE service_id = $1 AND source = $2`,
		serviceID, DiagnosticSource,
	).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !at.Valid {
		return 0, nil
	}
	return at.Int64, nil
}

// ApplyDiagnostics records one pull. Abnormal items and recoveries become
// timeline events. The pull does not open or resolve an incident.
func (db *DB) ApplyDiagnostics(pull DiagnosticPull) error {
	return db.ApplyDiagnosticsPresent(pull, nil)
}

// ApplyDiagnosticsPresent is ApplyDiagnostics with a presence check. present
// runs after the service lock. A false result returns ErrServiceGone and
// writes nothing. A nil present always writes.
func (db *DB) ApplyDiagnosticsPresent(pull DiagnosticPull, present func() bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockService(tx, pull.ServiceID); err != nil {
		return err
	}
	if present != nil && !present() {
		return ErrServiceGone
	}
	now := time.Now().Unix()
	if pull.ObservedAt == 0 {
		pull.ObservedAt = now
	}
	var raw []byte
	err = tx.QueryRow(
		`SELECT cursor FROM runtime_source_state WHERE service_id = $1 AND source = $2 FOR UPDATE`,
		pull.ServiceID, DiagnosticSource,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return err
	}
	cursor := diagnosticCursor{}
	if len(raw) > 0 && json.Unmarshal(raw, &cursor) != nil {
		cursor = diagnosticCursor{}
	}
	if pull.Success {
		if err := writeDiagnosticEvents(tx, pull, &cursor, now); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	var lastSuccess any
	if pull.Success {
		lastSuccess = pull.ObservedAt
	}
	_, err = tx.Exec(
		`INSERT INTO runtime_source_state (
			service_id, source, enabled, status, last_attempt_at, last_success_at,
			last_error_safe, cursor, updated_at
		 ) VALUES ($1, $2, TRUE, $3, $4, $5, $6, $7, $4)
		 ON CONFLICT (service_id, source) DO UPDATE SET
			enabled = TRUE,
			status = EXCLUDED.status,
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_success_at = COALESCE(EXCLUDED.last_success_at, runtime_source_state.last_success_at),
			last_error_safe = EXCLUDED.last_error_safe,
			cursor = CASE WHEN $8 THEN EXCLUDED.cursor ELSE runtime_source_state.cursor END,
			updated_at = EXCLUDED.updated_at`,
		pull.ServiceID, DiagnosticSource, pull.Status, pull.ObservedAt, lastSuccess, pull.Error, string(encoded), pull.Success,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func writeDiagnosticEvents(tx *sql.Tx, pull DiagnosticPull, cursor *diagnosticCursor, now int64) error {
	for _, item := range pull.Items {
		if item.Kind == "" || item.Name == "" || item.ObservedAt == 0 {
			continue
		}
		if diagnosticStatusKind(item.Kind) {
			prev := cursor.status(item.Kind, item.Name)
			if (prev == "degraded" || prev == "unhealthy") && (item.Status == "healthy" || item.Status == "unknown") {
				if err := insertDiagnosticEvent(tx, pull, item, true, now); err != nil {
					return err
				}
			}
			if item.Status == "degraded" || item.Status == "unhealthy" {
				if err := insertDiagnosticEvent(tx, pull, item, false, now); err != nil {
					return err
				}
			}
			cursor.set(item.Kind, item.Name, item.Status)
			continue
		}
		if err := insertDiagnosticEvent(tx, pull, item, false, now); err != nil {
			return err
		}
	}
	return nil
}

func insertDiagnosticEvent(tx *sql.Tx, pull DiagnosticPull, item DiagnosticItem, recovered bool, now int64) error {
	eventType := "diagnostics." + item.Kind
	title := item.Name
	severity := "info"
	key := diagnosticEventKey(pull.ServiceID, item)
	if recovered {
		eventType += ".recovered"
		title += " recovered"
		key += ":recovered"
	} else {
		switch {
		case item.Status == "unhealthy" || item.Kind == "recent_error":
			severity = "error"
		case item.Status == "degraded" || item.Kind == "slow_operation":
			severity = "warning"
		}
		if item.Status != "" {
			title = item.Name + " " + item.Status
		}
	}
	details, err := json.Marshal(map[string]any{
		"kind": item.Kind, "name": item.Name, "status": item.Status,
		"event_id": item.EventID, "attributes": item.Attributes, "detail": json.RawMessage(item.Detail),
	})
	if err != nil {
		return err
	}
	if legacy, ok := legacyDiagnosticKey(item, recovered); ok {
		stored, err := diagnosticLegacyHit(tx, pull.ServiceID, legacy)
		if err != nil {
			return err
		}
		if stored {
			return nil
		}
	}
	return insertTimelineTx(tx, timelineEvent{
		Key: key, ServiceID: pull.ServiceID, Source: DiagnosticSource, EventType: eventType,
		Severity: severity, Title: title, Summary: item.Summary,
		OccurredAt: item.ObservedAt, ObservedAt: pull.ObservedAt, Details: details,
	}, now)
}

// legacyDiagnosticKey is the event key written before the service id was part
// of the key. A later poll of the same service must not store that event again.
func legacyDiagnosticKey(item DiagnosticItem, recovered bool) (string, bool) {
	if item.EventID == "" {
		return "", false
	}
	key := "diagnostics:" + item.EventID
	if recovered {
		key += ":recovered"
	}
	return key, true
}

func diagnosticLegacyHit(tx *sql.Tx, serviceID, key string) (bool, error) {
	var owner string
	err := tx.QueryRow(`SELECT service_id FROM runtime_events WHERE event_key = $1`, key).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return owner == serviceID, nil
}

func diagnosticEventKey(serviceID string, item DiagnosticItem) string {
	if item.EventID != "" {
		// Length prefix keeps "svc:a"+"b" distinct from "svc"+"a:b".
		return fmt.Sprintf("diagnostics:%d:%s:%s", len(serviceID), serviceID, item.EventID)
	}
	body, err := json.Marshal(struct {
		ServiceID  string            `json:"service_id"`
		Kind       string            `json:"kind"`
		Name       string            `json:"name"`
		Status     string            `json:"status"`
		Summary    string            `json:"summary"`
		ObservedAt int64             `json:"observed_at"`
		Attributes map[string]string `json:"attributes"`
		Detail     json.RawMessage   `json:"detail"`
	}{
		ServiceID: serviceID, Kind: item.Kind, Name: item.Name, Status: item.Status,
		Summary: item.Summary, ObservedAt: item.ObservedAt, Attributes: item.Attributes, Detail: item.Detail,
	})
	if err != nil {
		body = []byte(serviceID + item.Kind + item.Name)
	}
	sum := sha256.Sum256(body)
	return "diagnostics:" + hex.EncodeToString(sum[:])
}

func diagnosticStatusKind(kind string) bool {
	switch kind {
	case "component_health", "dependency_health", "resource", "saturation":
		return true
	default:
		return false
	}
}

type diagnosticCursor struct {
	Items map[string]map[string]string `json:"items"`
}

func (c diagnosticCursor) status(kind, name string) string {
	if c.Items == nil {
		return ""
	}
	return c.Items[kind][name]
}

func (c *diagnosticCursor) set(kind, name, status string) {
	if c.Items == nil {
		c.Items = map[string]map[string]string{}
	}
	if c.Items[kind] == nil {
		c.Items[kind] = map[string]string{}
	}
	c.Items[kind][name] = status
}
