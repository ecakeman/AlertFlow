package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecakeman/alertflow/internal/event"
)

// ExternalAlertIn is one webhook body plus the service name and URL copied
// from config at accept time.
type ExternalAlertIn struct {
	Source      string
	EventID     string
	AlertKey    string
	ServiceID   string
	ServiceName string
	ServiceURL  string
	Status      string
	Title       string
	Description string
	OccurredAt  int64
}

// ExternalAlertView is one alert instance safe to show on an incident.
type ExternalAlertView struct {
	Source   string
	AlertKey string
	Status   string
	Title    string
}

// AcceptExternalAlert stores the inbound event and an outbox row in one
// transaction. A repeated source and event id returns duplicate and does not
// insert another outbox row. It does not open an incident or queue a delivery.
func (db *DB) AcceptExternalAlert(in ExternalAlertIn) (duplicate bool, err error) {
	if in.Source == "" || in.EventID == "" || in.AlertKey == "" || in.ServiceID == "" || in.OccurredAt <= 0 {
		return false, fmt.Errorf("external alert is missing source, event, key, service, or time")
	}
	if in.Status != "firing" && in.Status != "resolved" {
		return false, fmt.Errorf("external alert status must be firing or resolved")
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var id int64
	err = tx.QueryRow(
		`INSERT INTO external_alert_events (
			source, event_id, alert_key, service_id, status, occurred_at, received_at, title, description
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (source, event_id) DO NOTHING
		RETURNING id`,
		in.Source, in.EventID, in.AlertKey, in.ServiceID, in.Status, in.OccurredAt, now, in.Title, in.Description,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	ev := event.ExternalAlert{
		EventID:       event.ExternalOutboxID(in.Source, in.EventID),
		EventType:     event.TypeExternalAlert,
		SchemaVersion: event.SchemaVersion,
		OccurredAt:    in.OccurredAt,
		Source:        in.Source,
		CallerEventID: in.EventID,
		AlertKey:      in.AlertKey,
		ServiceID:     in.ServiceID,
		ServiceName:   in.ServiceName,
		ServiceURL:    in.ServiceURL,
		Status:        in.Status,
		Title:         in.Title,
		Description:   in.Description,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(
		`INSERT INTO outbox_events (
			event_id, event_type, schema_version, message_key, payload,
			created_at, attempt_count, next_attempt_at
		) VALUES ($1, $2, $3, $4, $5, $6, 0, $6)`,
		ev.EventID, ev.EventType, ev.SchemaVersion, ev.ServiceID, string(payload), now,
	)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

// ApplyExternalAlert updates the alert instance, the incident, the processed
// marker, and any pending notification tasks in one transaction.
func (db *DB) ApplyExternalAlert(ev event.ExternalAlert) error {
	if ev.EventType != event.TypeExternalAlert || ev.SchemaVersion != event.SchemaVersion {
		return fmt.Errorf("unsupported event %s version %d", ev.EventType, ev.SchemaVersion)
	}
	if ev.EventID == "" || ev.Source == "" || ev.AlertKey == "" || ev.ServiceID == "" {
		return fmt.Errorf("external alert is missing id, source, key, or service")
	}
	if ev.Status != "firing" && ev.Status != "resolved" {
		return fmt.Errorf("external alert status must be firing or resolved")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyExternalAlertTx(tx, ev); err != nil {
		if errors.Is(err, ErrAlreadyProcessed) {
			if cerr := tx.Commit(); cerr != nil {
				return cerr
			}
		}
		return err
	}
	return tx.Commit()
}

func applyExternalAlertTx(tx *sql.Tx, ev event.ExternalAlert) error {
	now := time.Now().Unix()
	var processedID int64
	err := tx.QueryRow(
		`INSERT INTO processed_events (consumer_name, event_id, event_type, processed_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (consumer_name, event_id) DO NOTHING
		 RETURNING id`,
		ConsumerName, ev.EventID, ev.EventType, now,
	).Scan(&processedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAlreadyProcessed
	}
	if err != nil {
		return err
	}
	httpStatus, err := lockRuntime(tx, ev.ServiceID, now)
	if err != nil {
		return err
	}
	var alertStatus, alertService string
	var lastOccurred int64
	err = tx.QueryRow(
		`SELECT status, service_id, last_occurred_at FROM external_alerts WHERE source = $1 AND alert_key = $2 FOR UPDATE`,
		ev.Source, ev.AlertKey,
	).Scan(&alertStatus, &alertService, &lastOccurred)
	exists := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		exists = false
		err = nil
	}
	if err != nil {
		return err
	}
	if exists && alertService != ev.ServiceID {
		return nil
	}
	// occurred_at is whole seconds. An equal timestamp does not prove which
	// event happened last, so only a strictly newer event may change state.
	if exists && ev.OccurredAt <= lastOccurred {
		return nil
	}

	subject := notifySubject{
		eventID: ev.EventID, serviceID: ev.ServiceID, serviceName: ev.ServiceName,
		serviceURL: ev.ServiceURL, occurredAt: ev.OccurredAt, attempts: 1, errText: ev.Description,
	}
	if ev.Status == "firing" {
		if exists && alertStatus == "firing" {
			_, err = tx.Exec(
				`UPDATE external_alerts
				 SET title = $3, description = $4, last_event_id = $5, last_occurred_at = $6, updated_at = $7
				 WHERE source = $1 AND alert_key = $2`,
				ev.Source, ev.AlertKey, ev.Title, ev.Description, ev.EventID, ev.OccurredAt, now,
			)
			return err
		}
		if exists {
			_, err = tx.Exec(
				`UPDATE external_alerts
				 SET service_id = $3, status = 'firing', title = $4, description = $5,
				     opened_at = $6, resolved_at = NULL, last_event_id = $7, last_occurred_at = $8, updated_at = $9
				 WHERE source = $1 AND alert_key = $2`,
				ev.Source, ev.AlertKey, ev.ServiceID, ev.Title, ev.Description, ev.OccurredAt, ev.EventID, ev.OccurredAt, now,
			)
		} else {
			_, err = tx.Exec(
				`INSERT INTO external_alerts (
					source, alert_key, service_id, status, title, description, opened_at, last_event_id, last_occurred_at, updated_at
				) VALUES ($1, $2, $3, 'firing', $4, $5, $6, $7, $8, $9)`,
				ev.Source, ev.AlertKey, ev.ServiceID, ev.Title, ev.Description, ev.OccurredAt, ev.EventID, ev.OccurredAt, now,
			)
		}
		if err != nil {
			return err
		}
		var existing int64
		err = tx.QueryRow(
			`SELECT id FROM incidents WHERE service_id = $1 AND status = 'ongoing'`,
			ev.ServiceID,
		).Scan(&existing)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		subject.startedAt = ev.OccurredAt
		id, err := insertOngoingTx(tx, ev.ServiceID, ev.OccurredAt, now, ev.Title)
		if err != nil {
			return err
		}
		return insertDeliveriesTx(tx, subject, "incident_start", &id, nil, now)
	}

	if !exists || alertStatus != "firing" {
		return nil
	}
	_, err = tx.Exec(
		`UPDATE external_alerts
		 SET status = 'resolved', resolved_at = $3, last_event_id = $4, last_occurred_at = $5, updated_at = $6
		 WHERE source = $1 AND alert_key = $2`,
		ev.Source, ev.AlertKey, ev.OccurredAt, ev.EventID, ev.OccurredAt, now,
	)
	if err != nil {
		return err
	}
	active, err := countFiringAlerts(tx, ev.ServiceID)
	if err != nil {
		return err
	}
	if httpStatus == StatusOffline || active > 0 {
		return nil
	}
	id, startedAt, err := resolveOngoingTx(tx, ev.ServiceID, ev.OccurredAt, now)
	if err != nil || id == 0 {
		return err
	}
	subject.startedAt = startedAt
	return insertDeliveriesTx(tx, subject, "incident_resolve", &id, &ev.OccurredAt, now)
}

func lockRuntime(tx *sql.Tx, serviceID string, now int64) (string, error) {
	if _, err := tx.Exec(
		`INSERT INTO service_runtime_state (service_id, current_status, last_event_id, last_check_id, last_event_at, updated_at)
		 VALUES ($1, 'online', '', 0, 0, $2)
		 ON CONFLICT (service_id) DO NOTHING`,
		serviceID, now,
	); err != nil {
		return "", err
	}
	var status string
	err := tx.QueryRow(
		`SELECT current_status FROM service_runtime_state WHERE service_id = $1 FOR UPDATE`,
		serviceID,
	).Scan(&status)
	return status, err
}

func countFiringAlerts(tx *sql.Tx, serviceID string) (int, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM external_alerts WHERE service_id = $1 AND status = 'firing'`,
		serviceID,
	).Scan(&n)
	return n, err
}

// ListExternalAlertsOverlapping returns alert instances for a service whose
// open interval meets [start, end].
func (db *DB) ListExternalAlertsOverlapping(serviceID string, start, end int64) ([]ExternalAlertView, error) {
	rows, err := db.Query(
		`SELECT source, alert_key, status, title
		 FROM external_alerts
		 WHERE service_id = $1
		   AND opened_at <= $3
		   AND (resolved_at IS NULL OR resolved_at >= $2)
		 ORDER BY opened_at, source, alert_key`,
		serviceID, start, end,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExternalAlertView
	for rows.Next() {
		var row ExternalAlertView
		if err := rows.Scan(&row.Source, &row.AlertKey, &row.Status, &row.Title); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
