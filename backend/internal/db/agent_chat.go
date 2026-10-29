package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// AgentWebhookType is the integration type reserved for the diagnostic agent.
// It lives here because the database needs it too: a chat request is queued as
// a delivery to this channel. internal/notify re-exports it.
const AgentWebhookType = "agent_webhook"

// EventChatRequested is the delivery event_type used for a queued chat
// question. It is not a notification event — only the agent channel handles it,
// and only chat requests are queued with it.
const EventChatRequested = "chat_requested"

// Chat request statuses.
const (
	ChatPending  = "pending"
	ChatAnswered = "answered"
	ChatFailed   = "failed"
)

// ChatRequestRow is one troubleshooting question and its answer.
type ChatRequestRow struct {
	ID          int64
	RequestID   string
	IncidentID  int64
	UserID      *int64
	Message     string
	Status      string
	DeliveryID  *int64
	Reply       *string
	EvidenceIDs json.RawMessage
	Error       string
	CreatedAt   int64
	UpdatedAt   int64
}

const chatRequestCols = `id, request_id, incident_id, user_id, message, status, delivery_id,
	reply, evidence_ids, error, created_at, updated_at`

// CreateChatRequest stores a question and queues it for the agent in one
// transaction. It returns created=false when request_id is already known, so a
// retried HTTP call cannot queue the question twice.
//
// The delivery row is what actually reaches the agent: it rides the same
// leases, retries and dead-lettering as a notification, so chat is not a second
// delivery mechanism. When there is no enabled agent_webhook channel the
// request is still stored (created=true, delivery_id NULL) and the caller can
// report that plainly instead of pretending it was sent.
func (db *DB) CreateChatRequest(rec ChatRequestRow, payload []byte, now int64) (*ChatRequestRow, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRow(
		`INSERT INTO agent_chat_requests (
			request_id, incident_id, user_id, message, status, created_at, updated_at
		 ) VALUES ($1, $2, $3, $4, 'pending', $5, $5)
		 ON CONFLICT (request_id) DO NOTHING
		 RETURNING id`,
		rec.RequestID, rec.IncidentID, rec.UserID, rec.Message, now,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Already queued. Return the stored row so the caller can report its
		// real status rather than an optimistic one.
		stored, getErr := getChatRequestTx(tx, rec.RequestID)
		if getErr != nil {
			return nil, false, getErr
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return stored, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	deliveryID, err := InsertInternalAgentTask(tx, rec.RequestID, &rec.IncidentID, EventChatRequested, payload, now)
	if err != nil {
		return nil, false, err
	}
	if deliveryID != 0 {
		if _, err := tx.Exec(
			`UPDATE agent_chat_requests SET delivery_id = $1, updated_at = $2 WHERE id = $3`,
			deliveryID, now, id,
		); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	stored, err := db.GetChatRequest(rec.RequestID)
	if err != nil {
		return nil, false, err
	}
	return stored, true, nil
}

// GetChatRequest returns a stored question by its correlation id.
func (db *DB) GetChatRequest(requestID string) (*ChatRequestRow, error) {
	return scanChatRequest(db.QueryRow(
		`SELECT `+chatRequestCols+` FROM agent_chat_requests WHERE request_id = $1`, requestID))
}

func getChatRequestTx(tx *sql.Tx, requestID string) (*ChatRequestRow, error) {
	return scanChatRequest(tx.QueryRow(
		`SELECT `+chatRequestCols+` FROM agent_chat_requests WHERE request_id = $1`, requestID))
}

// ListChatRequests returns an incident's questions, newest first.
func (db *DB) ListChatRequests(incidentID int64, limit int) ([]ChatRequestRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT `+chatRequestCols+` FROM agent_chat_requests
		 WHERE incident_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		incidentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatRequestRow
	for rows.Next() {
		row, err := scanChatRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *row)
	}
	return out, rows.Err()
}

// CompleteChatRequest records the agent's answer. A repeated callback changes
// nothing: the row is already answered, and the answer that won is kept.
// ApplyChatCallback stores a chat result and, when the incident has a service,
// the matching timeline row, in one transaction.
func (db *DB) ApplyChatCallback(messageID, messageType, requestID, reply string, evidenceIDs json.RawMessage, reason string, now int64) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var res sql.Result
	if messageType == "chat.failed" {
		res, err = tx.Exec(
			`UPDATE agent_chat_requests SET status = 'failed', error = $2, updated_at = $3
			 WHERE request_id = $1 AND status = 'pending'`,
			requestID, reason, now,
		)
	} else {
		res, err = tx.Exec(
			`UPDATE agent_chat_requests
			 SET status = 'answered', reply = $2, evidence_ids = $3::jsonb, error = '', updated_at = $4
			 WHERE request_id = $1 AND status = 'pending'`,
			requestID, reply, jsonOrNil(evidenceIDs), now,
		)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	var serviceID string
	var incidentID int64
	err = tx.QueryRow(
		`SELECT i.service_id, c.incident_id
		 FROM agent_chat_requests c
		 JOIN incidents i ON i.id = c.incident_id
		 WHERE c.request_id = $1`, requestID,
	).Scan(&serviceID, &incidentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if serviceID != "" {
		if err := insertAgentTimelineTx(tx, AgentRunRecord{
			MessageID: messageID, MessageType: messageType, ServiceID: serviceID,
			IncidentID: &incidentID, Error: reason, StartedAt: &now,
		}, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (db *DB) CompleteChatRequest(requestID, reply string, evidenceIDs json.RawMessage, now int64) (bool, error) {
	res, err := db.Exec(
		`UPDATE agent_chat_requests
		 SET status = 'answered', reply = $2, evidence_ids = $3::jsonb, error = '', updated_at = $4
		 WHERE request_id = $1 AND status = 'pending'`,
		requestID, reply, jsonOrNil(evidenceIDs), now,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// FailChatRequest records why a question produced no answer.
func (db *DB) FailChatRequest(requestID, reason string, now int64) (bool, error) {
	res, err := db.Exec(
		`UPDATE agent_chat_requests SET status = 'failed', error = $2, updated_at = $3
		 WHERE request_id = $1 AND status = 'pending'`,
		requestID, reason, now,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func scanChatRequest(row interface{ Scan(...any) error }) (*ChatRequestRow, error) {
	var rec ChatRequestRow
	var user, delivery sql.NullInt64
	var reply sql.NullString
	var evidence []byte
	if err := row.Scan(
		&rec.ID, &rec.RequestID, &rec.IncidentID, &user, &rec.Message, &rec.Status, &delivery,
		&reply, &evidence, &rec.Error, &rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if user.Valid {
		rec.UserID = &user.Int64
	}
	if delivery.Valid {
		rec.DeliveryID = &delivery.Int64
	}
	if reply.Valid {
		rec.Reply = &reply.String
	}
	rec.EvidenceIDs = jsonOrEmpty(evidence)
	return &rec, nil
}

// ChatExpireThreshold is how long a question may stay unanswered before it is
// reported as failed rather than left spinning forever.
const ChatExpireThreshold = 10 * time.Minute

// ExpireStaleChatRequests fails questions the agent never answered — a delivery
// that went dead, or an agent that restarted and lost the work. Without this a
// request would sit at 'pending' forever and the page would never stop waiting.
func (db *DB) ExpireStaleChatRequests(now int64) (int64, error) {
	res, err := db.Exec(
		`UPDATE agent_chat_requests
		 SET status = 'failed', error = 'no answer within the time limit', updated_at = $1
		 WHERE status = 'pending' AND created_at < $2`,
		now, now-int64(ChatExpireThreshold.Seconds()),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
