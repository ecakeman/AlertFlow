package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecakeman/alertflow/internal/event"
)

const ConsumerName = "alertflow-incident"

// ErrAlreadyProcessed means this consumer already applied the event.
var ErrAlreadyProcessed = errors.New("event already processed")

// DeliverySnapshot is the message a later sender will deliver. It has no secrets.
type DeliverySnapshot struct {
	Event       string `json:"event"`
	EventID     string `json:"eventId"`
	IncidentID  *int64 `json:"incidentId,omitempty"`
	ServiceID   string `json:"serviceId"`
	ServiceName string `json:"serviceName"`
	ServiceURL  string `json:"serviceUrl"`
	OccurredAt  int64  `json:"occurredAt"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	ResolvedAt  *int64 `json:"resolvedAt,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	Error       string `json:"error,omitempty"`
	CheckID     int64  `json:"checkId,omitempty"`
	// Source marks a manually created investigation. Check-driven tasks omit it.
	Source string `json:"source,omitempty"`
	// Chat is set only for a chat_requested delivery: a read-only question for
	// the diagnostic agent. It rides the notification delivery table so it
	// inherits the same leases, retries and dead-lettering.
	Chat *ChatSnapshot `json:"chat,omitempty"`
}

// ChatSnapshot is a troubleshooting question queued for the agent.
type ChatSnapshot struct {
	RequestID string          `json:"requestId"`
	Message   string          `json:"message"`
	History   json.RawMessage `json:"history,omitempty"`
}

// ApplyCheckEvent applies one check event: incident changes, the processed
// marker, runtime state, and pending notification tasks commit together.
// A repeated event returns ErrAlreadyProcessed and does not create more rows.
func (db *DB) ApplyCheckEvent(ev event.CheckCompleted) error {
	if ev.EventType != event.TypeCheckCompleted || ev.SchemaVersion != event.SchemaVersion {
		return fmt.Errorf("unsupported event %s version %d", ev.EventType, ev.SchemaVersion)
	}
	if ev.EventID == "" || ev.ServiceID == "" || ev.CheckID == 0 {
		return fmt.Errorf("event is missing id, service, or check id")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyCheckEventTx(tx, ev); err != nil {
		if errors.Is(err, ErrAlreadyProcessed) {
			if cerr := tx.Commit(); cerr != nil {
				return cerr
			}
		}
		return err
	}
	return tx.Commit()
}

func applyCheckEventTx(tx *sql.Tx, ev event.CheckCompleted) error {
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

	if _, err := tx.Exec(
		`INSERT INTO service_runtime_state (service_id, current_status, last_event_id, last_check_id, last_event_at, updated_at)
		 VALUES ($1, 'online', '', 0, 0, $2)
		 ON CONFLICT (service_id) DO NOTHING`,
		ev.ServiceID, now,
	); err != nil {
		return err
	}
	var status string
	var lastCheck int64
	if err := tx.QueryRow(
		`SELECT current_status, last_check_id FROM service_runtime_state WHERE service_id = $1 FOR UPDATE`,
		ev.ServiceID,
	).Scan(&status, &lastCheck); err != nil {
		return err
	}
	// A newer check already won. Remember this event so it is not applied again,
	// but do not rewind the incident or the runtime status.
	if lastCheck > 0 && ev.CheckID < lastCheck {
		return nil
	}

	prev := status
	if lastCheck == 0 {
		prev, err = previousStatus(tx, ev.ServiceID, ev.CheckID)
		if err != nil {
			return err
		}
	}

	var incidentID *int64
	var resolvedAt *int64
	kind := ""
	subject := notifySubject{
		eventID: ev.EventID, serviceID: ev.ServiceID, serviceName: ev.ServiceName,
		serviceURL: ev.ServiceURL, occurredAt: ev.OccurredAt, attempts: ev.Attempts, errText: ev.Error,
		checkID: ev.CheckID,
	}
	switch event.Decide(prev, ev.Status) {
	case event.TransitionOpen:
		var existing int64
		err := tx.QueryRow(
			`SELECT id FROM incidents WHERE service_id = $1 AND status = 'ongoing'`,
			ev.ServiceID,
		).Scan(&existing)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, err := insertOngoingTx(tx, ev.ServiceID, ev.OccurredAt, now, "")
		if err != nil {
			return err
		}
		incidentID = &id
		subject.startedAt = ev.OccurredAt
		kind = "incident_start"
	case event.TransitionResolve:
		id, startedAt, err := resolveOngoingTx(tx, ev.ServiceID, ev.OccurredAt, now)
		if err != nil {
			return err
		}
		if id != 0 {
			incidentID = &id
			resolvedAt = &ev.OccurredAt
			subject.startedAt = startedAt
			kind = "incident_resolve"
		}
	case event.TransitionWarning:
		id, err := insertWarningTx(tx, ev.ServiceID, ev.OccurredAt, now)
		if err != nil {
			return err
		}
		incidentID = &id
		kind = "warning"
	}

	if kind == "incident_start" || kind == "incident_resolve" {
		evType, title, severity := timelineOpened, "Incident opened", "error"
		if kind == "incident_resolve" {
			evType, title, severity = timelineResolved, "Incident resolved", "info"
		}
		key := incidentOpenedKey(*incidentID)
		if kind == "incident_resolve" {
			key = incidentResolvedKey(*incidentID)
		}
		if err := insertTimelineTx(tx, timelineEvent{
			Key: key, ServiceID: ev.ServiceID, EventType: evType, Severity: severity,
			Title: title, Summary: ev.ServiceName, OccurredAt: ev.OccurredAt, ObservedAt: ev.OccurredAt,
			IncidentID: *incidentID, Origin: "check",
		}, now); err != nil {
			return err
		}
	}
	if kind != "" {
		if err := insertDeliveriesTx(tx, subject, kind, incidentID, resolvedAt, now); err != nil {
			return err
		}
	}
	_, err = tx.Exec(
		`UPDATE service_runtime_state
		 SET current_status = $2, last_event_id = $3, last_check_id = $4, last_event_at = $5, updated_at = $6
		 WHERE service_id = $1`,
		ev.ServiceID, ev.Status, ev.EventID, ev.CheckID, ev.OccurredAt, now,
	)
	return err
}

func previousStatus(tx *sql.Tx, serviceID string, beforeCheckID int64) (string, error) {
	var ongoing int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM incidents WHERE service_id = $1 AND status = 'ongoing'`,
		serviceID,
	).Scan(&ongoing); err != nil {
		return "", err
	}
	if ongoing > 0 {
		return StatusOffline, nil
	}
	var status string
	err := tx.QueryRow(
		`SELECT status FROM checks WHERE service_id = $1 AND id < $2 ORDER BY id DESC LIMIT 1`,
		serviceID, beforeCheckID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return StatusOnline, nil
	}
	if err != nil {
		return "", err
	}
	if status == "" {
		return StatusOnline, nil
	}
	return status, nil
}

func insertOngoingTx(tx *sql.Tx, serviceID string, startedAt, now int64, title string) (int64, error) {
	var titleVal any
	if title != "" {
		titleVal = title
	}
	var id int64
	err := tx.QueryRow(
		`INSERT INTO incidents (service_id, status, source, title, started_at, resolved_at, created_by, created_at, updated_at)
		 VALUES ($1, 'ongoing', 'auto', $4, $2, NULL, NULL, $3, $3) RETURNING id`,
		serviceID, startedAt, now, titleVal,
	).Scan(&id)
	if isUniqueViolation(err) {
		return 0, ErrOngoingExists
	}
	return id, err
}

func resolveOngoingTx(tx *sql.Tx, serviceID string, resolvedAt, now int64) (int64, int64, error) {
	var id, startedAt int64
	err := tx.QueryRow(
		`UPDATE incidents SET status = 'resolved', resolved_at = $2, updated_at = $3
		 WHERE service_id = $1 AND status = 'ongoing'
		 RETURNING id, started_at`,
		serviceID, resolvedAt, now,
	).Scan(&id, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return id, startedAt, err
}

func insertWarningTx(tx *sql.Tx, serviceID string, ts, now int64) (int64, error) {
	var id int64
	err := tx.QueryRow(
		`INSERT INTO incidents (service_id, status, source, severity, title, started_at, resolved_at, created_by, created_at, updated_at)
		 VALUES ($1, 'resolved', 'auto', 'warning', NULL, $2, $2, NULL, $3, $3) RETURNING id`,
		serviceID, ts, now,
	).Scan(&id)
	return id, err
}

type notifySubject struct {
	eventID     string
	serviceID   string
	serviceName string
	serviceURL  string
	occurredAt  int64
	startedAt   int64
	attempts    int
	errText     string
	checkID     int64
}

func insertDeliveriesTx(tx *sql.Tx, ev notifySubject, kind string, incidentID, resolvedAt *int64, now int64) error {
	rows, err := tx.Query(
		`SELECT id, notify_warnings FROM integrations WHERE enabled AND type = $1`,
		AgentWebhookType,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type channel struct {
		id       int64
		warnings bool
	}
	var channels []channel
	for rows.Next() {
		var c channel
		if err := rows.Scan(&c.id, &c.warnings); err != nil {
			return err
		}
		channels = append(channels, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range channels {
		if kind == "warning" && !c.warnings {
			continue
		}
		startedAt := ev.startedAt
		resolved := resolvedAt
		if kind == "warning" {
			startedAt = 0
			resolved = nil
		}
		snap := DeliverySnapshot{
			Event:       kind,
			EventID:     ev.eventID,
			IncidentID:  incidentID,
			ServiceID:   ev.serviceID,
			ServiceName: ev.serviceName,
			ServiceURL:  ev.serviceURL,
			OccurredAt:  ev.occurredAt,
			StartedAt:   startedAt,
			ResolvedAt:  resolved,
			Attempts:    ev.attempts,
			Error:       ev.errText,
			CheckID:     ev.checkID,
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO notification_deliveries (
				event_id, incident_id, integration_id, event_type, payload, status,
				attempt_count, next_attempt_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, 'pending', 0, $6, $6, $6)`,
			ev.eventID, incidentID, c.id, kind, string(raw), now,
		); err != nil {
			return err
		}
	}
	return nil
}

// RecordDeadLetter keeps an unreadable Kafka message so the consumer can move past it.
func (db *DB) RecordDeadLetter(topic string, partition int, offset int64, eventID, eventType string, payload []byte, reason string) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	_, err := db.Exec(
		`INSERT INTO dead_letter_events (topic, partition_id, offset_value, event_id, event_type, payload, error, created_at)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8)
		 ON CONFLICT (topic, partition_id, offset_value) DO NOTHING`,
		topic, partition, offset, eventID, eventType, string(payload), reason, time.Now().Unix(),
	)
	return err
}
