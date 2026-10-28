package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// ErrServiceGone means the on-disk config no longer contains the service, so
// the write was dropped after the service lock was taken.
var ErrServiceGone = errors.New("service is no longer configured")

// RecordCheckAndOutbox inserts one check and its outbox event in a single
// transaction. The event payload's check_id is the id returned by that insert.
// A failure, including Commit, leaves neither row behind.
func (db *DB) RecordCheckAndOutbox(ev event.CheckCompleted) (int64, error) {
	return db.RecordCheckAndOutboxPresent(ev, nil, nil)
}

// RecordCheckAndOutboxPresent is RecordCheckAndOutbox with a presence check.
// present runs after the service lock and before the insert. A false result
// rolls the transaction back and returns ErrServiceGone. A nil present always
// writes. cert, when set, is stored in the same transaction as the check.
func (db *DB) RecordCheckAndOutboxPresent(ev event.CheckCompleted, present func() bool, cert *ServiceTLS) (int64, error) {
	if ev.EventID == "" || ev.EventType == "" || ev.ServiceID == "" {
		return 0, fmt.Errorf("record check: event is missing id, type, or service")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockService(tx, ev.ServiceID); err != nil {
		return 0, err
	}
	if present != nil && !present() {
		return 0, ErrServiceGone
	}
	epoch, err := ensureServiceEpoch(tx, ev.ServiceID)
	if err != nil {
		return 0, err
	}
	ev.Epoch = &epoch

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
	if cert != nil {
		if err := upsertServiceTLSTx(tx, ev.ServiceID, cert); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return checkID, nil
}

// ServiceRetired reports whether this event belongs to a service life that was
// already deleted. An event that carries an epoch is retired when that epoch
// is not newer than the stored one. An older payload, with no epoch, is retired
// when its occurred_at is not newer than retired_at.
func (db *DB) ServiceRetired(serviceID string, occurredAt int64, epoch *int64) (bool, error) {
	retiredAt, retiredEpoch, ok, err := retiredMark(db, serviceID)
	if err != nil || !ok {
		return false, err
	}
	return eventIsRetired(occurredAt, epoch, retiredAt, retiredEpoch), nil
}

func retiredMark(q interface {
	QueryRow(string, ...any) *sql.Row
}, serviceID string) (int64, int64, bool, error) {
	var retiredAt, epoch int64
	err := q.QueryRow(`SELECT retired_at, epoch FROM retired_services WHERE service_id = $1`, serviceID).Scan(&retiredAt, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return retiredAt, epoch, true, nil
}

func eventIsRetired(occurredAt int64, epoch *int64, retiredAt, retiredEpoch int64) bool {
	if epoch != nil {
		return *epoch <= retiredEpoch
	}
	return occurredAt <= retiredAt
}

// epochAtDelete is the life being removed. A missing live row uses the retired
// epoch, or 0 when the service was never recorded.
func epochAtDelete(tx *sql.Tx, serviceID string) (int64, error) {
	var epoch int64
	err := tx.QueryRow(`SELECT epoch FROM service_epochs WHERE service_id = $1`, serviceID).Scan(&epoch)
	if err == nil {
		return epoch, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRow(`SELECT epoch FROM retired_services WHERE service_id = $1`, serviceID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return epoch, err
}

// ensureServiceEpoch returns the life a new check belongs to, creating the row
// when this is the first check after a delete.
func ensureServiceEpoch(tx *sql.Tx, serviceID string) (int64, error) {
	var epoch int64
	err := tx.QueryRow(`SELECT epoch FROM service_epochs WHERE service_id = $1`, serviceID).Scan(&epoch)
	if err == nil {
		return epoch, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var retired sql.NullInt64
	err = tx.QueryRow(`SELECT epoch FROM retired_services WHERE service_id = $1`, serviceID).Scan(&retired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	epoch = 0
	if retired.Valid {
		epoch = retired.Int64 + 1
	}
	if _, err := tx.Exec(`INSERT INTO service_epochs (service_id, epoch) VALUES ($1, $2)`, serviceID, epoch); err != nil {
		return 0, err
	}
	return epoch, nil
}

// ErrAckUnrecorded means the broker accepted the message and this transaction
// could not store published_at. The send is not rolled back. Retry may publish
// the same event again; the consumer drops the duplicate.
var ErrAckUnrecorded = errors.New("broker ack was not recorded")

// PublishUnderServiceLock holds the same service advisory lock as
// DeleteServiceRecords across the Kafka send. The lock is taken before the
// outbox row. send runs only while that lock is held. A retired service or a
// lost lease returns false and does not call send. A send error rolls the
// transaction back and leaves the unpublished row in place.
func (db *DB) PublishUnderServiceLock(serviceID string, id int64, owner string, occurredAt int64, epoch *int64, send func() error) (bool, error) {
	if serviceID == "" || id == 0 || owner == "" || send == nil {
		return false, fmt.Errorf("publish: missing service, outbox id, owner, or sender")
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockService(tx, serviceID); err != nil {
		return false, err
	}
	var leaseOwner sql.NullString
	var leaseUntil sql.NullInt64
	var published bool
	err = tx.QueryRow(
		`SELECT lease_owner, lease_until, published_at IS NOT NULL
		 FROM outbox_events WHERE id = $1 FOR UPDATE`, id,
	).Scan(&leaseOwner, &leaseUntil, &published)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	leased := !published && leaseOwner.String == owner && leaseUntil.Valid && leaseUntil.Int64 > now
	retiredAt, retiredEpoch, retiredOK, err := retiredMark(tx, serviceID)
	if err != nil {
		return false, err
	}
	retired := retiredOK && eventIsRetired(occurredAt, epoch, retiredAt, retiredEpoch)
	if !leased || retired {
		if retired && leased {
			if _, err := tx.Exec(`DELETE FROM outbox_events WHERE id = $1 AND published_at IS NULL`, id); err != nil {
				return false, err
			}
			if err := tx.Commit(); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if err := send(); err != nil {
		return false, err
	}
	res, err := tx.Exec(
		`UPDATE outbox_events
		 SET published_at = $2, lease_owner = NULL, lease_until = NULL
		 WHERE id = $1 AND published_at IS NULL AND lease_owner = $3 AND lease_until > $4`,
		id, time.Now().Unix(), owner, now,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, ErrAckUnrecorded
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%w: %v", ErrAckUnrecorded, err)
	}
	return true, nil
}

// ForgetLeasedOutbox deletes a claimed row that must not be published.
// A row already removed by service deletion is not an error.
func (db *DB) ForgetLeasedOutbox(id int64, owner string, now int64) error {
	_, err := db.Exec(
		`DELETE FROM outbox_events
		 WHERE id = $1 AND published_at IS NULL AND lease_owner = $2 AND lease_until > $3`,
		id, owner, now,
	)
	return err
}

// lockService takes a transaction-scoped advisory lock for one service.
// Check writes, diagnostic writes, service deletion and outbox publication all
// use it. Publication holds it until the Kafka send returns.
func lockService(tx *sql.Tx, serviceID string) error {
	_, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, serviceID)
	return err
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
