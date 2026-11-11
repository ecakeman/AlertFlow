package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EpisodeView 是 Episode 与其任务进度。
type EpisodeView struct {
	EpisodeID             uuid.UUID  `json:"episode_id"`
	ServiceID             uuid.UUID  `json:"service_id"`
	ServiceName           string     `json:"service_name"`
	Status                string     `json:"status"`
	ResolutionPending     bool       `json:"resolution_pending"`
	StartedBy             string     `json:"started_by"`
	StartedAt             time.Time  `json:"started_at"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ArchivedAt            *time.Time `json:"archived_at,omitempty"`
	InputHash             string     `json:"input_hash"`
	InputVersion          int64      `json:"input_version"`
	ProcessedInputVersion int64      `json:"processed_input_version"`
	FinalInputVersion     *int64     `json:"final_input_version,omitempty"`
	MonitorStatus         string     `json:"monitor_status"`
	AppStatus             string     `json:"app_status"`
	ActiveTaskID          *uuid.UUID `json:"active_task_id,omitempty"`
	ActiveTaskStatus      string     `json:"active_task_status,omitempty"`
	ActiveTaskType        string     `json:"active_task_type,omitempty"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

func (s *Store) GetEpisode(ctx context.Context, id uuid.UUID) (EpisodeView, error) {
	var v EpisodeView
	var resolved, archived sql.NullTime
	var final sql.NullInt64
	var taskID uuid.NullUUID
	var taskStatus, taskType sql.NullString
	err := s.DB.QueryRowContext(ctx, `
		SELECT e.episode_id, e.service_id, sv.name, e.status, e.resolution_pending, e.started_by, e.started_at,
		       e.resolved_at, e.archived_at, e.input_hash, e.input_version, e.processed_input_version,
		       e.final_input_version, m.status, a.status, t.task_id, t.status, t.task_type, e.updated_at
		FROM system.episodes e
		JOIN system.services sv ON sv.service_id=e.service_id
		JOIN system.service_source_states m ON m.service_id=e.service_id AND m.source='MONITOR'
		JOIN system.service_source_states a ON a.service_id=e.service_id AND a.source='APP'
		LEFT JOIN system.agent_tasks t ON t.episode_id=e.episode_id
		  AND t.status IN ('PENDING','DISPATCHED','RUNNING','RETRY_WAIT')
		WHERE e.episode_id=$1`, id).Scan(
		&v.EpisodeID, &v.ServiceID, &v.ServiceName, &v.Status, &v.ResolutionPending, &v.StartedBy, &v.StartedAt,
		&resolved, &archived, &v.InputHash, &v.InputVersion, &v.ProcessedInputVersion, &final,
		&v.MonitorStatus, &v.AppStatus, &taskID, &taskStatus, &taskType, &v.UpdatedAt)
	if err == sql.ErrNoRows {
		return EpisodeView{}, errCode("EPISODE_NOT_FOUND", "episode not found", 404)
	}
	if err != nil {
		return EpisodeView{}, err
	}
	if resolved.Valid {
		v.ResolvedAt = &resolved.Time
	}
	if archived.Valid {
		v.ArchivedAt = &archived.Time
	}
	if final.Valid {
		v.FinalInputVersion = &final.Int64
	}
	if taskID.Valid {
		v.ActiveTaskID = &taskID.UUID
		v.ActiveTaskStatus = taskStatus.String
		v.ActiveTaskType = taskType.String
	}
	return v, nil
}

// TimelineRecord 是管理查询里的一条时间轴记录。
type TimelineRecord struct {
	RecordType string          `json:"record_type"`
	RecordID   string          `json:"record_id"`
	EventTime  time.Time       `json:"event_time"`
	ReceivedAt time.Time       `json:"received_at"`
	Source     string          `json:"source"`
	Content    json.RawMessage `json:"content"`
}

func (s *Store) ServiceTimeline(ctx context.Context, serviceID uuid.UUID, limit int) ([]TimelineRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT point_id, observed_at, received_at, check_status, diagnostic_fields
		FROM system.service_monitor_points
		WHERE service_id=$1
		ORDER BY point_seq DESC
		LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineRecord
	for rows.Next() {
		var id uuid.UUID
		var rec TimelineRecord
		var check string
		var fields []byte
		if err := rows.Scan(&id, &rec.EventTime, &rec.ReceivedAt, &check, &fields); err != nil {
			return nil, err
		}
		rec.RecordType = "MONITOR_POINT"
		rec.RecordID = id.String()
		rec.Source = SourceMonitor
		rec.Content, _ = json.Marshal(map[string]any{"check_status": check, "diagnostic_fields": json.RawMessage(fields)})
		out = append(out, rec)
	}
	return out, rows.Err()
}

// EpisodeTimeline 返回三块逻辑时间轴：监控投影、应用事件、产物引用。
func (s *Store) EpisodeTimeline(ctx context.Context, episodeID uuid.UUID) (map[string]any, error) {
	ep, err := s.GetEpisode(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	var proj []byte
	var start sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `
		SELECT current_monitor_projection, monitor_start_point_seq FROM system.episodes WHERE episode_id=$1`,
		episodeID).Scan(&proj, &start); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT event_record_id, event_id, event_kind, COALESCE(health_status,''), event_time, received_at, payload
		FROM system.episode_app_events WHERE episode_id=$1 ORDER BY event_seq`, episodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []map[string]any
	for rows.Next() {
		var recordID uuid.UUID
		var eventID, kind, health string
		var eventTime, received time.Time
		var payload []byte
		if err := rows.Scan(&recordID, &eventID, &kind, &health, &eventTime, &received, &payload); err != nil {
			return nil, err
		}
		events = append(events, map[string]any{
			"event_record_id": recordID, "event_id": eventID, "event_kind": kind,
			"health_status": health, "event_time": eventTime, "received_at": received,
			"payload": json.RawMessage(payload),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refRows, err := s.DB.QueryContext(ctx, `
		SELECT artifact_type, artifact_version, storage_key, content_type, content_hash, updated_at
		FROM system.episode_artifact_refs WHERE episode_id=$1`, episodeID)
	if err != nil {
		return nil, err
	}
	defer refRows.Close()
	var refs []map[string]any
	for refRows.Next() {
		var typ, key, ctype, hash string
		var ver int64
		var updated time.Time
		if err := refRows.Scan(&typ, &ver, &key, &ctype, &hash, &updated); err != nil {
			return nil, err
		}
		refs = append(refs, map[string]any{
			"artifact_type": typ, "artifact_version": ver, "storage_key": key,
			"content_type": ctype, "content_hash": hash, "updated_at": updated,
		})
	}
	return map[string]any{
		"episode":                 ep,
		"region1_projection":      json.RawMessage(proj),
		"monitor_start_point_seq": start.Int64,
		"region2_events":          events,
		"region3_refs":            refs,
	}, refRows.Err()
}

// InputSnapshot 是固定输入版本的快照。
type InputSnapshot struct {
	EpisodeID      uuid.UUID
	ServiceID      uuid.UUID
	ServiceName    string
	InputVersion   int64
	InputHash      string
	Projection     []byte
	PointWatermark sql.NullInt64
	EventWatermark sql.NullInt64
	StartedAt      time.Time
	MonitorStart   sql.NullInt64
	Generation     int64
	TaskType       string
	VersionFrom    sql.NullInt64
	VersionTo      sql.NullInt64
}

func (s *Store) EpisodeInput(ctx context.Context, taskID, episodeID uuid.UUID, version, generation int64) (InputSnapshot, error) {
	var snap InputSnapshot
	err := s.DB.QueryRowContext(ctx, `
		SELECT e.episode_id, e.service_id, sv.name, v.input_version, v.input_hash, v.monitor_projection,
		       v.monitor_point_watermark, v.app_event_watermark, e.started_at, e.monitor_start_point_seq,
		       t.execution_generation, t.task_type, t.input_version_from, t.input_version_to
		FROM system.agent_tasks t
		JOIN system.episodes e ON e.episode_id=t.episode_id
		JOIN system.services sv ON sv.service_id=e.service_id
		JOIN system.episode_input_versions v ON v.episode_id=e.episode_id AND v.input_version=$4
		WHERE t.task_id=$1 AND t.episode_id=$2 AND t.execution_generation=$3
		  AND t.task_type='DIAGNOSIS' AND t.input_version_to=$4`,
		taskID, episodeID, generation, version).Scan(
		&snap.EpisodeID, &snap.ServiceID, &snap.ServiceName, &snap.InputVersion, &snap.InputHash, &snap.Projection,
		&snap.PointWatermark, &snap.EventWatermark, &snap.StartedAt, &snap.MonitorStart,
		&snap.Generation, &snap.TaskType, &snap.VersionFrom, &snap.VersionTo)
	if err == sql.ErrNoRows {
		return InputSnapshot{}, errCode("INPUT_NOT_VISIBLE", "input version is outside this task", 409)
	}
	return snap, err
}

// QueryTimeline 只返回该任务 input_version_to 水位以内的记录。
func (s *Store) QueryTimeline(ctx context.Context, taskID, episodeID uuid.UUID, version, generation int64, start, end time.Time, pointIDs, eventIDs []string) ([]TimelineRecord, error) {
	snap, err := s.EpisodeInput(ctx, taskID, episodeID, version, generation)
	if err != nil {
		return nil, err
	}
	if !end.After(start) {
		return nil, errCode("VALIDATION", "end_time must be after start_time", 400)
	}
	var records []TimelineRecord
	pointQuery := `
		SELECT point_id::text, observed_at, received_at, check_status, raw_result, diagnostic_fields
		FROM system.service_monitor_points
		WHERE service_id=$1
		  AND point_seq >= COALESCE($2::bigint, 0)
		  AND ($3::bigint IS NULL OR point_seq <= $3)
		  AND observed_at >= $4 AND observed_at <= $5`
	args := []any{snap.ServiceID, snap.MonitorStart, snap.PointWatermark, start, end}
	if len(pointIDs) > 0 {
		pointQuery += ` AND point_id::text = ANY($6)`
		args = append(args, pointIDs)
	}
	rows, err := s.DB.QueryContext(ctx, pointQuery, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rec TimelineRecord
		var check string
		var raw, fields []byte
		if err := rows.Scan(&rec.RecordID, &rec.EventTime, &rec.ReceivedAt, &check, &raw, &fields); err != nil {
			rows.Close()
			return nil, err
		}
		rec.RecordType = "MONITOR_POINT"
		rec.Source = SourceMonitor
		rec.Content, _ = json.Marshal(map[string]any{"check_status": check, "raw_result": json.RawMessage(raw), "diagnostic_fields": json.RawMessage(fields)})
		records = append(records, rec)
	}
	rows.Close()
	eventQuery := `
		SELECT event_record_id::text, event_time, received_at, event_kind, COALESCE(health_status,''), payload
		FROM system.episode_app_events
		WHERE episode_id=$1
		  AND ($2::bigint IS NULL OR event_seq <= $2)
		  AND event_time >= $3 AND event_time <= $4`
	eargs := []any{episodeID, snap.EventWatermark, start, end}
	if len(eventIDs) > 0 {
		eventQuery += ` AND event_record_id::text = ANY($5)`
		eargs = append(eargs, eventIDs)
	}
	erows, err := s.DB.QueryContext(ctx, eventQuery, eargs...)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var rec TimelineRecord
		var kind, health string
		var payload []byte
		if err := erows.Scan(&rec.RecordID, &rec.EventTime, &rec.ReceivedAt, &kind, &health, &payload); err != nil {
			return nil, err
		}
		rec.RecordType = "APP_EVENT"
		rec.Source = SourceApp
		rec.Content, _ = json.Marshal(map[string]any{"event_kind": kind, "health_status": health, "payload": json.RawMessage(payload)})
		records = append(records, rec)
	}
	return records, erows.Err()
}

// DueTarget 是调度器要执行的目标。
type DueTarget struct {
	ServiceID  uuid.UUID
	TargetID   uuid.UUID
	TargetType string
	Address    string
	Config     json.RawMessage
	Interval   time.Duration
	Timeout    time.Duration
}

func (s *Store) ListDueTargets(ctx context.Context) ([]DueTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.service_id, t.target_id, t.target_type, t.address, t.config, t.interval_ms, t.timeout_ms
		FROM system.service_monitor_targets t
		JOIN system.services s ON s.service_id=t.service_id
		WHERE t.enabled AND s.enabled AND t.deleted_at IS NULL AND s.deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueTarget
	for rows.Next() {
		var d DueTarget
		var interval, timeout int
		if err := rows.Scan(&d.ServiceID, &d.TargetID, &d.TargetType, &d.Address, &d.Config, &interval, &timeout); err != nil {
			return nil, err
		}
		d.Interval = time.Duration(interval) * time.Millisecond
		d.Timeout = time.Duration(timeout) * time.Millisecond
		out = append(out, d)
	}
	return out, rows.Err()
}
