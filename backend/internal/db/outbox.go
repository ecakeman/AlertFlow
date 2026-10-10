package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ecakeman/alertflow/internal/event"
)

// OutboxEvent is one unpublished (or later published) check event.
// PublishedAt stays nil until a publisher records a real broker acknowledgement.
type OutboxEvent struct {
	ID            int64
	EventID       string
	EventType     string
	SchemaVersion int
	MessageKey    string
	Payload       json.RawMessage
	CreatedAt     int64
	PublishedAt   *int64
	AttemptCount  int
	NextAttemptAt int64
	LastError     string
	LeaseOwner    string
	LeaseUntil    *int64
}

// RecordCheckAndOutbox inserts one check and its outbox event in a single
// transaction. The event payload's check_id is the id returned by that insert.
// A failure, including Commit, leaves neither row behind.
func (db *DB) RecordCheckAndOutbox(ev event.CheckCompleted) (int64, error) {
	if ev.EventID == "" || ev.EventType == "" || ev.ServiceID == "" {
		return 0, fmt.Errorf("record check: event is missing id, type, or service")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	attempts := ev.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var errVal any
	if ev.Error != "" {
		errVal = ev.Error
	}
	var checkID int64
	err = tx.QueryRow(
		`INSERT INTO checks (service_id, ts, status, latency_ms, status_code, error, attempts)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		ev.ServiceID, ev.OccurredAt, ev.Status, ev.LatencyMs, ev.StatusCode, errVal, attempts,
	).Scan(&checkID)
	if err != nil {
		return 0, err
	}
	ev.CheckID = checkID
	payload, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(
		`INSERT INTO outbox_events (
			event_id, event_type, schema_version, message_key, payload,
			created_at, attempt_count, next_attempt_at
		) VALUES ($1, $2, $3, $4, $5, $6, 0, $7)`,
		ev.EventID, ev.EventType, ev.SchemaVersion, ev.ServiceID, string(payload), ev.OccurredAt, ev.OccurredAt,
	)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return checkID, nil
}

// GetOutboxByEventID loads one outbox row. The stored event_id is the stable id.
func (db *DB) GetOutboxByEventID(eventID string) (*OutboxEvent, error) {
	row := db.QueryRow(`SELECT `+outboxCols+` FROM outbox_events WHERE event_id = $1`, eventID)
	ev, err := scanOutbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

const outboxCols = `id, event_id, event_type, schema_version, message_key, payload,
	created_at, published_at, attempt_count, next_attempt_at, last_error, lease_owner, lease_until`

func scanOutbox(row interface{ Scan(dest ...any) error }) (*OutboxEvent, error) {
	var ev OutboxEvent
	var payload []byte
	var published, leaseUntil sql.NullInt64
	var lastError, leaseOwner sql.NullString
	err := row.Scan(
		&ev.ID, &ev.EventID, &ev.EventType, &ev.SchemaVersion, &ev.MessageKey, &payload,
		&ev.CreatedAt, &published, &ev.AttemptCount, &ev.NextAttemptAt, &lastError, &leaseOwner, &leaseUntil,
	)
	if err != nil {
		return nil, err
	}
	ev.Payload = json.RawMessage(payload)
	if published.Valid {
		ev.PublishedAt = &published.Int64
	}
	if leaseUntil.Valid {
		ev.LeaseUntil = &leaseUntil.Int64
	}
	ev.LastError = lastError.String
	ev.LeaseOwner = leaseOwner.String
	return &ev, nil
}

// ClaimOutbox leases up to limit pending events in one short transaction.
// The lock is FOR UPDATE SKIP LOCKED on outbox rows only. It does not lock
// services, which still live in config.yaml.
func (db *DB) ClaimOutbox(owner string, now, leaseUntil int64, limit int) ([]OutboxEvent, error) {
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
			SELECT id FROM outbox_events
			WHERE published_at IS NULL
			  AND next_attempt_at <= $3
			  AND (lease_until IS NULL OR lease_until <= $3)
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		)
		UPDATE outbox_events AS o
		SET lease_owner = $1, lease_until = $2
		FROM picked
		WHERE o.id = picked.id
		RETURNING o.id, o.event_id, o.event_type, o.schema_version, o.message_key, o.payload,
			o.created_at, o.published_at, o.attempt_count, o.next_attempt_at, o.last_error, o.lease_owner, o.lease_until`,
		owner, leaseUntil, now, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		ev, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordOutboxFailure stores a delivery error and the next time to try.
// The row stays unpublished and is not deleted. owner must still hold the lease.
func (db *DB) RecordOutboxFailure(id int64, owner string, now int64, errMsg string, nextAttemptAt int64) error {
	res, err := db.Exec(
		`UPDATE outbox_events
		 SET attempt_count = attempt_count + 1,
		     last_error = $2,
		     next_attempt_at = $3,
		     lease_owner = NULL,
		     lease_until = NULL
		 WHERE id = $1 AND published_at IS NULL AND lease_owner = $4 AND lease_until > $5`,
		id, errMsg, nextAttemptAt, owner, now,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkOutboxPublished records that this lease owner observed a broker acknowledgement.
// A publisher whose lease was taken by someone else updates nothing.
func (db *DB) MarkOutboxPublished(id int64, owner string, now, publishedAt int64) error {
	res, err := db.Exec(
		`UPDATE outbox_events
		 SET published_at = $2, lease_owner = NULL, lease_until = NULL
		 WHERE id = $1 AND published_at IS NULL AND lease_owner = $3 AND lease_until > $4`,
		id, publishedAt, owner, now,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
