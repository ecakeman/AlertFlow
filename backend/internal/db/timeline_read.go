package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// RuntimeEvent is one timeline row. Details is the stored JSON object.
type RuntimeEvent struct {
	ID           int64
	EventKey     string
	ServiceID    string
	Source       string
	EventType    string
	Severity     string
	Title        string
	Summary      string
	OccurredAt   int64
	ObservedAt   int64
	RecordedAt   int64
	IncidentID   sql.NullInt64
	ClusterID    sql.NullString
	Namespace    sql.NullString
	ResourceKind sql.NullString
	ResourceName sql.NullString
	ResourceUID  sql.NullString
	Details      json.RawMessage
}

// RuntimeSource is one collector's stored health. Cursor is not part of the read model.
type RuntimeSource struct {
	Source      string
	Status      string
	Enabled     bool
	LastSuccess sql.NullInt64
	LastError   string
}

// TimelineFilter is a keyset page of one service's events. OccurredAt is
// inclusive on both ends. A cursor excludes that row and anything newer.
type TimelineFilter struct {
	ServiceID string
	From      int64
	To        int64
	Sources   []string
	Types     []string
	CursorAt  int64
	CursorID  int64
	HasCursor bool
	Limit     int
}

// ListRuntimeEvents returns events newest first.
func (db *DB) ListRuntimeEvents(f TimelineFilter) ([]RuntimeEvent, error) {
	if f.Limit < 1 {
		f.Limit = 1
	}
	args := []any{f.ServiceID, f.From, f.To}
	q := `SELECT id, event_key, service_id, source, event_type, severity, title, summary,
		occurred_at, observed_at, recorded_at, incident_id,
		cluster_id, namespace, resource_kind, resource_name, resource_uid, details
		FROM runtime_events
		WHERE service_id = $1 AND occurred_at >= $2 AND occurred_at <= $3`
	if len(f.Sources) > 0 {
		args = append(args, f.Sources)
		q += fmt.Sprintf(" AND source = ANY($%d::text[])", len(args))
	}
	if len(f.Types) > 0 {
		args = append(args, f.Types)
		q += fmt.Sprintf(" AND event_type = ANY($%d::text[])", len(args))
	}
	if f.HasCursor {
		args = append(args, f.CursorAt, f.CursorID)
		q += fmt.Sprintf(" AND (occurred_at, id) < ($%d::bigint, $%d::bigint)", len(args)-1, len(args))
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf(" ORDER BY occurred_at DESC, id DESC LIMIT $%d", len(args))
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuntimeEvent
	for rows.Next() {
		var ev RuntimeEvent
		if err := rows.Scan(
			&ev.ID, &ev.EventKey, &ev.ServiceID, &ev.Source, &ev.EventType, &ev.Severity, &ev.Title, &ev.Summary,
			&ev.OccurredAt, &ev.ObservedAt, &ev.RecordedAt, &ev.IncidentID,
			&ev.ClusterID, &ev.Namespace, &ev.ResourceKind, &ev.ResourceName, &ev.ResourceUID, &ev.Details,
		); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListRuntimeSources returns collector rows for one service.
func (db *DB) ListRuntimeSources(serviceID string) ([]RuntimeSource, error) {
	rows, err := db.Query(
		`SELECT source, status, enabled, last_success_at, last_error_safe
		 FROM runtime_source_state WHERE service_id = $1`, serviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuntimeSource
	for rows.Next() {
		var row RuntimeSource
		if err := rows.Scan(&row.Source, &row.Status, &row.Enabled, &row.LastSuccess, &row.LastError); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
