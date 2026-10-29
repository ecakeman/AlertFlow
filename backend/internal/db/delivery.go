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

// ErrChannelRetired means the delivery belongs to a channel that no longer sends.
var ErrChannelRetired = errors.New("notification channel is retired")

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

const exhaustedReason = "attempt limit reached before the delivery finished"

// ClaimDeliveries leases up to limit due tasks for agent_webhook only. Telegram,
// Slack, email and generic webhook rows are left untouched: they are not claimed,
// retried, or swept to dead, even when the integration is enabled again.
// Disabled agent channels are left alone so they can be sent after the channel
// is enabled again. Expired running leases are claimable until maxAttempts. A
// task already at that limit is marked dead in this transaction and is not
// claimed again. That transition writes no notification_log row.
func (db *DB) ClaimDeliveries(owner string, now, leaseUntil int64, limit, maxAttempts int) ([]ClaimedDelivery, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE notification_deliveries AS d
		 SET status = 'dead',
		     lease_owner = NULL,
		     lease_until = NULL,
		     last_error = $1,
		     updated_at = $2
		 FROM (
		   SELECT d2.id
		   FROM notification_deliveries d2
		   WHERE d2.integration_id IS NULL
		     AND (
		       (d2.status = 'running' AND d2.lease_until <= $2 AND d2.attempt_count >= $3)
		       OR (d2.status IN ('pending', 'retry_wait') AND d2.next_attempt_at <= $2 AND d2.attempt_count >= $3)
		     )
		   FOR UPDATE OF d2 SKIP LOCKED
		 ) AS picked
		 WHERE d.id = picked.id`,
		exhaustedReason, now, maxAttempts,
	); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`WITH picked AS (
			SELECT d.id
			FROM notification_deliveries d
			WHERE d.integration_id IS NULL
			  AND d.attempt_count < $5
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
		owner, leaseUntil, now, limit, maxAttempts,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claimed []ClaimedDelivery
	for rows.Next() {
		var row ClaimedDelivery
		var incident, integration sql.NullInt64
		var payload []byte
		if err := rows.Scan(&row.ID, &row.EventID, &incident, &integration, &row.EventType, &payload, &row.AttemptCount); err != nil {
			return nil, err
		}
		if incident.Valid {
			row.IncidentID = &incident.Int64
		}
		if integration.Valid {
			row.IntegrationID = integration.Int64
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
		if claimed[i].IntegrationID == 0 {
			continue
		}
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

// InsertInternalAgentTask queues one gRPC investigation or chat task.
// A repeated event id and type is ignored. The row has no integration.
func InsertInternalAgentTask(tx *sql.Tx, eventID string, incidentID *int64, eventType string, payload []byte, now int64) (int64, error) {
	var id int64
	err := tx.QueryRow(
		`INSERT INTO notification_deliveries (
			event_id, incident_id, integration_id, event_type, payload, status,
			attempt_count, next_attempt_at, created_at, updated_at
		 ) VALUES ($1, $2, NULL, $3, $4, 'pending', 0, $5, $5, $5)
		 ON CONFLICT (event_id, event_type) WHERE integration_id IS NULL DO NOTHING
		 RETURNING id`,
		eventID, incidentID, eventType, string(payload), now,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (db *DB) finishDelivery(id int64, owner string, now int64, status, errMsg string, stamp int64, logStatus string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var eventType string
	var integrationID sql.NullInt64
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
	if incident.Valid && eventType != "warning" {
		incidentID = incident.Int64
	}
	if integrationID.Valid {
		if _, err := tx.Exec(
			`INSERT INTO notification_log (integration_id, incident_id, event, status, error, sent_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			integrationID.Int64, incidentID, eventType, logStatus, nullStr(errMsg), now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDeliveriesByIncident returns tasks for one incident, oldest first.
func (db *DB) ListDeliveriesByIncident(incidentID int64) ([]DeliveryView, error) {
	rows, err := db.Query(
		`SELECT d.id, d.event_id, d.incident_id, d.integration_id, i.name, i.type,
		        d.event_type, d.status, d.attempt_count, d.next_attempt_at, d.last_error, d.sent_at
		 FROM notification_deliveries d
		 LEFT JOIN integrations i ON i.id = d.integration_id
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
		var incident, sent, integration sql.NullInt64
		var last, name, kind sql.NullString
		if err := rows.Scan(
			&row.ID, &row.EventID, &incident, &integration, &name, &kind,
			&row.EventType, &row.Status, &row.AttemptCount, &row.NextAttemptAt, &last, &sent,
		); err != nil {
			return nil, err
		}
		if incident.Valid {
			row.IncidentID = &incident.Int64
		}
		if integration.Valid {
			row.IntegrationID = integration.Int64
		}
		row.IntegrationName = name.String
		row.IntegrationType = kind.String
		if sent.Valid {
			row.SentAt = &sent.Int64
		}
		row.LastError = last.String
		out = append(out, row)
	}
	return out, rows.Err()
}

// RetryDeadDelivery starts a new attempt round for a dead agent_webhook task.
// The same row is reused, so the event and integration stay unique. A dead task
// on a retired channel is refused.
func (db *DB) RetryDeadDelivery(id, now int64) error {
	res, err := db.Exec(
		`UPDATE notification_deliveries AS d
		 SET status = 'pending', attempt_count = 0, next_attempt_at = $2,
		     last_error = NULL, lease_owner = NULL, lease_until = NULL, updated_at = $2
		 WHERE d.id = $1 AND d.status = 'dead'
		   AND (
		     d.integration_id IS NULL
		     OR EXISTS (
		       SELECT 1 FROM integrations i
		       WHERE i.id = d.integration_id AND i.type = $3
		     )
		   )`,
		id, now, AgentWebhookType,
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
	var kind sql.NullString
	err = db.QueryRow(
		`SELECT d.status, i.type
		 FROM notification_deliveries d
		 LEFT JOIN integrations i ON i.id = d.integration_id
		 WHERE d.id = $1`, id,
	).Scan(&status, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if kind.Valid && kind.String != AgentWebhookType {
		return ErrChannelRetired
	}
	return ErrNotRetryable
}

const (
	retiredCancelReason  = "retired channel cancelled after old delivery workers were drained"
	retiredRunningReason = "running delivery left in place; an old worker may still send"
)

// RetiredDeliveryResult is what one drain of the retired channels changed.
type RetiredDeliveryResult struct {
	Cancelled int64
	Audited   int64
}

// CancelRetiredDeliveries marks pending and retry_wait tasks on telegram, slack,
// email and webhook as cancelled. Running rows stay running and get one audit
// row, because this process cannot prove an old worker will not still send.
// Calling it again does not insert a second audit row. It does not run from
// server startup.
func (db *DB) CancelRetiredDeliveries(now int64) (RetiredDeliveryResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return RetiredDeliveryResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO retired_delivery_audits (
			delivery_id, event_id, integration_id, integration_type, status, reason, created_at
		 )
		 SELECT d.id, d.event_id, d.integration_id, i.type, d.status, $2, $1
		 FROM notification_deliveries d
		 JOIN integrations i ON i.id = d.integration_id
		 WHERE i.type IN ('telegram', 'slack', 'email', 'webhook')
		   AND d.status IN ('pending', 'retry_wait')
		 ON CONFLICT (delivery_id) DO NOTHING`,
		now, retiredCancelReason,
	); err != nil {
		return RetiredDeliveryResult{}, err
	}
	res, err := tx.Exec(
		`UPDATE notification_deliveries AS d
		 SET status = 'cancelled',
		     last_error = $2,
		     lease_owner = NULL,
		     lease_until = NULL,
		     updated_at = $1
		 FROM integrations i
		 WHERE i.id = d.integration_id
		   AND i.type IN ('telegram', 'slack', 'email', 'webhook')
		   AND d.status IN ('pending', 'retry_wait')`,
		now, retiredCancelReason,
	)
	if err != nil {
		return RetiredDeliveryResult{}, err
	}
	cancelled, err := res.RowsAffected()
	if err != nil {
		return RetiredDeliveryResult{}, err
	}
	res, err = tx.Exec(
		`INSERT INTO retired_delivery_audits (
			delivery_id, event_id, integration_id, integration_type, status, reason, created_at
		 )
		 SELECT d.id, d.event_id, d.integration_id, i.type, d.status, $2, $1
		 FROM notification_deliveries d
		 JOIN integrations i ON i.id = d.integration_id
		 WHERE i.type IN ('telegram', 'slack', 'email', 'webhook')
		   AND d.status = 'running'
		 ON CONFLICT (delivery_id) DO NOTHING`,
		now, retiredRunningReason,
	)
	if err != nil {
		return RetiredDeliveryResult{}, err
	}
	audited, err := res.RowsAffected()
	if err != nil {
		return RetiredDeliveryResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RetiredDeliveryResult{}, err
	}
	return RetiredDeliveryResult{Cancelled: cancelled, Audited: audited}, nil
}
