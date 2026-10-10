package db

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// ErrLeaseLost means this worker no longer holds the delivery lease.
var ErrLeaseLost = errors.New("delivery lease lost")

// ErrNotRetryable means the delivery is not in the dead state.
var ErrNotRetryable = errors.New("delivery is not dead")

// ClaimedDelivery is one task leased to a worker. Config is the integration
// secret and must not be written back onto the delivery payload.
type ClaimedDelivery struct {
	ID              int64
	EventID         string
	IncidentID      *int64
	IntegrationID   int64
	IntegrationType string
	Config          json.RawMessage
	EventType       string
	Payload         json.RawMessage
	AttemptCount    int
}

// DeliveryView is a delivery row safe to show in the API. It has no secrets.
type DeliveryView struct {
	ID              int64
	EventID         string
	IncidentID      *int64
	IntegrationID   int64
	IntegrationName string
	IntegrationType string
	EventType       string
	Status          string
	AttemptCount    int
	NextAttemptAt   int64
	LastError       string
	SentAt          *int64
}

// ClaimDeliveries leases up to limit due tasks. Disabled integrations are left
// alone so they can be sent after the channel is enabled again. Expired running
// leases are claimable.
func (db *DB) ClaimDeliveries(owner string, now, leaseUntil int64, limit int) ([]ClaimedDelivery, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`WITH picked AS (
			SELECT d.id
			FROM notification_deliveries d
			JOIN integrations i ON i.id = d.integration_id
			WHERE i.enabled
			  AND (
			    (d.status IN ('pending', 'retry_wait') AND d.next_attempt_at <= $3)
			    OR (d.status = 'running' AND d.lease_until <= $3)
			  )
			ORDER BY d.id
			FOR UPDATE OF d SKIP LOCKED
			LIMIT $4
		)
		UPDATE notification_deliveries AS d
		SET status = 'running',
		    lease_owner = $1,
		    lease_until = $2,
		    attempt_count = d.attempt_count + 1,
		    updated_at = $3
		FROM picked
		WHERE d.id = picked.id
		RETURNING d.id, d.event_id, d.incident_id, d.integration_id, d.event_type, d.payload, d.attempt_count`,
		owner, leaseUntil, now, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claimed []ClaimedDelivery
	for rows.Next() {
		var row ClaimedDelivery
		var incident sql.NullInt64
		var payload []byte
		if err := rows.Scan(&row.ID, &row.EventID, &incident, &row.IntegrationID, &row.EventType, &payload, &row.AttemptCount); err != nil {
			return nil, err
		}
		if incident.Valid {
			row.IncidentID = &incident.Int64
		}
		row.Payload = json.RawMessage(payload)
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range claimed {
		var cfg []byte
		var kind string
		if err := tx.QueryRow(
			`SELECT type, config FROM integrations WHERE id = $1`, claimed[i].IntegrationID,
		).Scan(&kind, &cfg); err != nil {
			return nil, err
		}
		claimed[i].IntegrationType = kind
		claimed[i].Config = json.RawMessage(cfg)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

// SucceedDelivery marks a leased task sent and appends a success log row.
// A lost lease changes nothing and does not write a success log.
func (db *DB) SucceedDelivery(id int64, owner string, now, sentAt int64) error {
	return db.finishDelivery(id, owner, now, "succeeded", "", sentAt, "sent")
}

// FailDelivery records a leased failure as retry_wait or dead, plus a failed log row.
func (db *DB) FailDelivery(id int64, owner string, now int64, status, errMsg string, nextAttemptAt int64) error {
	if status != "retry_wait" && status != "dead" {
		return errors.New("delivery failure status must be retry_wait or dead")
	}
	return db.finishDelivery(id, owner, now, status, errMsg, nextAttemptAt, "failed")
}

func (db *DB) finishDelivery(id int64, owner string, now int64, status, errMsg string, stamp int64, logStatus string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var eventType string
	var integrationID int64
	var incident sql.NullInt64
	if status == "succeeded" {
		err = tx.QueryRow(
			`UPDATE notification_deliveries
			 SET status = 'succeeded', sent_at = $4, last_error = NULL,
			     lease_owner = NULL, lease_until = NULL, updated_at = $3
			 WHERE id = $1 AND status = 'running' AND lease_owner = $2 AND lease_until > $3
			 RETURNING event_type, integration_id, incident_id`,
			id, owner, now, stamp,
		).Scan(&eventType, &integrationID, &incident)
	} else {
		err = tx.QueryRow(
			`UPDATE notification_deliveries
			 SET status = $4, last_error = $5, next_attempt_at = $6,
			     lease_owner = NULL, lease_until = NULL, updated_at = $3
			 WHERE id = $1 AND status = 'running' AND lease_owner = $2 AND lease_until > $3
			 RETURNING event_type, integration_id, incident_id`,
			id, owner, now, status, errMsg, stamp,
		).Scan(&eventType, &integrationID, &incident)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	var incidentID any
	if incident.Valid {
		incidentID = incident.Int64
	}
	if _, err := tx.Exec(
		`INSERT INTO notification_log (integration_id, incident_id, event, status, error, sent_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		integrationID, incidentID, eventType, logStatus, nullStr(errMsg), now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ListDeliveriesByIncident returns tasks for one incident, oldest first.
func (db *DB) ListDeliveriesByIncident(incidentID int64) ([]DeliveryView, error) {
	rows, err := db.Query(
		`SELECT d.id, d.event_id, d.incident_id, d.integration_id, i.name, i.type,
		        d.event_type, d.status, d.attempt_count, d.next_attempt_at, d.last_error, d.sent_at
		 FROM notification_deliveries d
		 JOIN integrations i ON i.id = d.integration_id
		 WHERE d.incident_id = $1
		 ORDER BY d.id`,
		incidentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryView
	for rows.Next() {
		var row DeliveryView
		var incident, sent sql.NullInt64
		var last sql.NullString
		if err := rows.Scan(
			&row.ID, &row.EventID, &incident, &row.IntegrationID, &row.IntegrationName, &row.IntegrationType,
			&row.EventType, &row.Status, &row.AttemptCount, &row.NextAttemptAt, &last, &sent,
		); err != nil {
			return nil, err
		}
		if incident.Valid {
			row.IncidentID = &incident.Int64
		}
		if sent.Valid {
			row.SentAt = &sent.Int64
		}
		row.LastError = last.String
		out = append(out, row)
	}
	return out, rows.Err()
}

// RetryDeadDelivery starts a new attempt round for a dead task. The same row is
// reused, so the event and integration stay unique.
func (db *DB) RetryDeadDelivery(id, now int64) error {
	res, err := db.Exec(
		`UPDATE notification_deliveries
		 SET status = 'pending', attempt_count = 0, next_attempt_at = $2,
		     last_error = NULL, lease_owner = NULL, lease_until = NULL, updated_at = $2
		 WHERE id = $1 AND status = 'dead'`,
		id, now,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var status string
	err = db.QueryRow(`SELECT status FROM notification_deliveries WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return ErrNotRetryable
}
