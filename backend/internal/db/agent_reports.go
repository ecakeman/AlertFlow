package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// AgentRunRecord is one diagnostic result the agent pushed back over
// POST /webhooks/agent. Report, Evidence and Usage are the JSON blobs exactly
// as received; AlertFlow does not reinterpret them.
type AgentRunRecord struct {
	ID            int64
	MessageID     string
	RunID         string
	MessageType   string
	CorrelationID string
	IncidentID    *int64
	ServiceID     string
	EventType     string
	Mode          string
	Status        string
	Report        json.RawMessage
	Evidence      json.RawMessage
	Usage         json.RawMessage
	Error         string
	StartedAt     *int64
	FinishedAt    *int64
	CreatedAt     int64
	UpdatedAt     int64
}

const agentRunCols = `id, message_id, run_id, message_type, correlation_id, incident_id, service_id,
	event_type, mode, status, report, evidence, usage, error, started_at, finished_at,
	created_at, updated_at`

// SaveAgentRun stores one agent result and reports whether it was inserted.
//
// message_id is unique, so redelivering the same message changes nothing and
// returns inserted=false — that is the idempotency the retrying agent relies
// on. A write failure returns an error instead of succeeding quietly, so the
// caller can answer 5xx and the agent will send it again.
func (db *DB) SaveAgentRun(rec AgentRunRecord) (bool, error) {
	now := time.Now().Unix()
	res, err := db.Exec(`
		INSERT INTO agent_runs (
			message_id, run_id, message_type, correlation_id, incident_id, service_id,
			event_type, mode, status, report, evidence, usage, error, started_at, finished_at,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12::jsonb,$13,$14,$15,$16,$16)
		ON CONFLICT (message_id) DO NOTHING`,
		rec.MessageID, rec.RunID, rec.MessageType, rec.CorrelationID, rec.IncidentID, rec.ServiceID,
		rec.EventType, rec.Mode, rec.Status, jsonOrNil(rec.Report), jsonOrNil(rec.Evidence),
		jsonOrNil(rec.Usage), rec.Error, rec.StartedAt, rec.FinishedAt, now,
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

// LatestAgentRunForIncident returns the newest result reported for an incident,
// or (nil, nil) when the agent has reported nothing for it.
func (db *DB) LatestAgentRunForIncident(incidentID int64) (*AgentRunRecord, error) {
	row := db.QueryRow(
		`SELECT `+agentRunCols+` FROM agent_runs WHERE incident_id = $1
		 ORDER BY updated_at DESC, id DESC LIMIT 1`, incidentID)

	var rec AgentRunRecord
	var incident, started, finished sql.NullInt64
	var report, evidence, usage []byte
	if err := row.Scan(
		&rec.ID, &rec.MessageID, &rec.RunID, &rec.MessageType, &rec.CorrelationID, &incident,
		&rec.ServiceID, &rec.EventType, &rec.Mode, &rec.Status, &report, &evidence, &usage,
		&rec.Error, &started, &finished, &rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if incident.Valid {
		rec.IncidentID = &incident.Int64
	}
	if started.Valid {
		rec.StartedAt = &started.Int64
	}
	if finished.Valid {
		rec.FinishedAt = &finished.Int64
	}
	rec.Report = jsonOrEmpty(report)
	rec.Evidence = jsonOrEmpty(evidence)
	rec.Usage = jsonOrEmpty(usage)
	return &rec, nil
}

// jsonOrNil passes empty JSON through as SQL NULL rather than a made-up "{}",
// so "the agent did not send a report" stays distinguishable from "it sent an
// empty one".
func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func jsonOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}
