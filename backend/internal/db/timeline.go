package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	timelineSourceIncident = "incident"
	timelineOpened         = "incident.opened"
	timelineResolved       = "incident.resolved"
)

type timelineEvent struct {
	Key        string
	ServiceID  string
	EventType  string
	Severity   string
	Title      string
	Summary    string
	OccurredAt int64
	ObservedAt int64
	IncidentID int64
	Origin     string
	// Source defaults to the incident source. Details, when set, replaces the
	// origin object.
	Source  string
	Details json.RawMessage
	ClusterID    string
	Namespace    string
	ResourceKind string
	ResourceName string
	ResourceUID  string
}

func incidentOpenedKey(id int64) string {
	return fmt.Sprintf("incident:%d:opened", id)
}

func incidentResolvedKey(id int64) string {
	return fmt.Sprintf("incident:%d:resolved", id)
}

func insertTimelineTx(tx *sql.Tx, ev timelineEvent, now int64) error {
	source := ev.Source
	if source == "" {
		source = timelineSourceIncident
	}
	details := ev.Details
	if len(details) == 0 {
		raw, err := json.Marshal(map[string]string{"origin": ev.Origin})
		if err != nil {
			return err
		}
		details = raw
	}
	var incidentID any
	if ev.IncidentID != 0 {
		incidentID = ev.IncidentID
	}
	_, err := tx.Exec(
		`INSERT INTO runtime_events (
			event_key, service_id, source, event_type, severity, title, summary,
			occurred_at, observed_at, recorded_at, incident_id,
			cluster_id, namespace, resource_kind, resource_name, resource_uid,
			details
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		 ON CONFLICT (event_key) DO NOTHING`,
		ev.Key, ev.ServiceID, source, ev.EventType, ev.Severity, ev.Title, ev.Summary,
		ev.OccurredAt, ev.ObservedAt, now, incidentID,
		nullStr(ev.ClusterID), nullStr(ev.Namespace), nullStr(ev.ResourceKind),
		nullStr(ev.ResourceName), nullStr(ev.ResourceUID), string(details),
	)
	return err
}

// ManualIncident is one administrator-created outage and, when an agent target
// is usable, one investigation task per enabled agent_webhook.
type ManualIncident struct {
	ServiceID   string
	ServiceName string
	ServiceURL  string
	Title       *string
	StartedAt   int64
	ResolvedAt  *int64
	CreatedBy   *int64
}

// CreateManualIncident writes the incident, its timeline events, and any
// investigation tasks in one transaction.
func (db *DB) CreateManualIncident(in ManualIncident) (*Incident, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var id int64
	err = tx.QueryRow(
		`INSERT INTO incidents (service_id, status, source, title, started_at, resolved_at, created_by, created_at, updated_at)
		 VALUES ($1, 'ongoing', 'manual', $2, $3, NULL, $4, $5, $5) RETURNING id`,
		in.ServiceID, in.Title, in.StartedAt, in.CreatedBy, now,
	).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrOngoingExists
	}
	if err != nil {
		return nil, err
	}
	if err := insertTimelineTx(tx, timelineEvent{
		Key: incidentOpenedKey(id), ServiceID: in.ServiceID, EventType: timelineOpened,
		Severity: "error", Title: "Incident opened", Summary: in.ServiceName,
		OccurredAt: in.StartedAt, ObservedAt: now, IncidentID: id, Origin: "manual",
	}, now); err != nil {
		return nil, err
	}
	if in.ResolvedAt != nil {
		if _, err := tx.Exec(
			`UPDATE incidents SET status = 'resolved', resolved_at = $1, updated_at = $2 WHERE id = $3`,
			*in.ResolvedAt, now, id,
		); err != nil {
			return nil, err
		}
		if err := insertTimelineTx(tx, timelineEvent{
			Key: incidentResolvedKey(id), ServiceID: in.ServiceID, EventType: timelineResolved,
			Severity: "info", Title: "Incident resolved", Summary: in.ServiceName,
			OccurredAt: *in.ResolvedAt, ObservedAt: now, IncidentID: id, Origin: "manual",
		}, now); err != nil {
			return nil, err
		}
	}
	if err := insertManualDeliveriesTx(tx, in, id, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetIncident(id)
}

func insertManualDeliveriesTx(tx *sql.Tx, in ManualIncident, incidentID, now int64) error {
	rows, err := tx.Query(
		`SELECT id, config FROM integrations WHERE enabled AND type = $1 ORDER BY id`,
		AgentWebhookType,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type channel struct {
		id  int64
		cfg []byte
	}
	var channels []channel
	for rows.Next() {
		var c channel
		if err := rows.Scan(&c.id, &c.cfg); err != nil {
			return err
		}
		if usableAgentURL(c.cfg) {
			channels = append(channels, c)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	eventID := incidentOpenedKey(incidentID)
	for _, c := range channels {
		raw, err := json.Marshal(DeliverySnapshot{
			Event: "incident_start", EventID: eventID, IncidentID: &incidentID,
			ServiceID: in.ServiceID, ServiceName: in.ServiceName, ServiceURL: in.ServiceURL,
			OccurredAt: in.StartedAt, StartedAt: in.StartedAt, ResolvedAt: in.ResolvedAt,
			Source: "manual",
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO notification_deliveries (
				event_id, incident_id, integration_id, event_type, payload, status,
				attempt_count, next_attempt_at, created_at, updated_at
			) VALUES ($1, $2, $3, 'incident_start', $4, 'pending', 0, $5, $5, $5)`,
			eventID, incidentID, c.id, string(raw), now,
		); err != nil {
			return err
		}
	}
	return nil
}

func usableAgentURL(raw []byte) bool {
	var cfg struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

// TimelineRetentionDays is 7, 15, or 30. Any other value, including unset, is 30.
func TimelineRetentionDays() int {
	switch os.Getenv("ALERTFLOW_TIMELINE_RETENTION_DAYS") {
	case "7":
		return 7
	case "15":
		return 15
	default:
		return 30
	}
}

// DeleteTimelineBefore removes timeline events older than cutoff. Source and
// object state rows are not expired by the check retention setting.
func (db *DB) DeleteTimelineBefore(cutoff int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM runtime_events WHERE occurred_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
