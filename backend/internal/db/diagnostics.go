package db

import (
	"database/sql"
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
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	key := fmt.Sprintf("diagnostics:%s:%s:%s:%d", pull.ServiceID, item.Kind, item.Name, item.ObservedAt)
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
	details, err := json.Marshal(map[string]string{
		"kind": item.Kind, "name": item.Name, "status": item.Status,
	})
	if err != nil {
		return err
	}
	return insertTimelineTx(tx, timelineEvent{
		Key: key, ServiceID: pull.ServiceID, Source: DiagnosticSource, EventType: eventType,
		Severity: severity, Title: title, Summary: item.Summary,
		OccurredAt: item.ObservedAt, ObservedAt: pull.ObservedAt, Details: details,
	}, now)
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
