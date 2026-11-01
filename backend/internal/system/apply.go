package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// MonitorSample 是一次已经完成的探测。调度器与测试都走这条事务。
type MonitorSample struct {
	ServiceID   uuid.UUID
	TargetID    uuid.UUID
	ObservedAt  time.Time
	Success     bool
	StatusClass string
	ErrorClass  string
	Raw         json.RawMessage
}

// ApplyResult 是一次监控或应用输入后的 Episode 结果。
type ApplyResult struct {
	PointID        uuid.UUID `json:"point_id,omitempty"`
	EpisodeID      uuid.UUID `json:"episode_id,omitempty"`
	EpisodeStatus  string    `json:"episode_status,omitempty"`
	InputVersion   int64     `json:"input_version,omitempty"`
	MonitorStatus  string    `json:"monitor_status,omitempty"`
	AppStatus      string    `json:"app_status,omitempty"`
	CreatedEpisode bool      `json:"created_episode,omitempty"`
}

type episodeRow struct {
	ID                    uuid.UUID
	ServiceID             uuid.UUID
	Status                string
	ResolutionPending     bool
	StartedBy             string
	StartedAt             time.Time
	MonitorStart          sql.NullInt64
	MonitorWatermark      sql.NullInt64
	Projection            []MonitorField
	InputHash             string
	InputVersion          int64
	ProcessedInputVersion int64
	FinalInputVersion     sql.NullInt64
}

// ApplyMonitorSample 写入 Service Timeline，更新目标与 MONITOR 来源，并在需要时维护 Episode。
func (s *Store) ApplyMonitorSample(ctx context.Context, sample MonitorSample) (ApplyResult, error) {
	var out ApplyResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockService(ctx, tx, sample.ServiceID); err != nil {
			return err
		}
		var targetType, targetKey string
		var failTh, recTh int
		var required, enabled bool
		err := tx.QueryRowContext(ctx, `
			SELECT target_type, target_key, failure_threshold, recovery_threshold, is_required, enabled
			FROM system.service_monitor_targets
			WHERE target_id=$1 AND service_id=$2`, sample.TargetID, sample.ServiceID).
			Scan(&targetType, &targetKey, &failTh, &recTh, &required, &enabled)
		if err == sql.ErrNoRows {
			return errCode("TARGET_NOT_FOUND", "monitor target not found", 404)
		}
		if err != nil {
			return err
		}
		if !enabled {
			return errCode("TARGET_DISABLED", "monitor target is disabled", 409)
		}
		prevMonitor, err := sourceStatus(ctx, tx, sample.ServiceID, SourceMonitor)
		if err != nil {
			return err
		}
		var status string
		var fails, oks int
		err = tx.QueryRowContext(ctx, `
			SELECT status, consecutive_failures, consecutive_successes
			FROM system.service_monitor_target_states WHERE target_id=$1 FOR UPDATE`, sample.TargetID).
			Scan(&status, &fails, &oks)
		if err != nil {
			return err
		}
		now := s.now()
		if sample.ObservedAt.IsZero() {
			sample.ObservedAt = now
		}
		if sample.Success {
			oks++
			fails = 0
			if oks >= recTh {
				status = StatusNormal
			}
		} else {
			fails++
			oks = 0
			if fails >= failTh {
				status = StatusAbnormal
			}
		}
		fields := map[string]string{"status_class": sample.StatusClass, "error_class": sample.ErrorClass}
		rawFields, _ := json.Marshal(fields)
		raw := sample.Raw
		if len(raw) == 0 {
			raw = []byte(`{}`)
		}
		checkStatus := "FAILURE"
		if sample.Success {
			checkStatus = "SUCCESS"
		}
		pointID := uuid.New()
		var pointSeq int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO system.service_monitor_points
			(point_id, service_id, target_id, observed_at, received_at, check_status, raw_result, diagnostic_fields)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING point_seq`,
			pointID, sample.ServiceID, sample.TargetID, sample.ObservedAt.UTC(), now, checkStatus, raw, rawFields).Scan(&pointSeq)
		if err != nil {
			return err
		}
		lastOK, lastFail := sql.NullTime{}, sql.NullTime{}
		if sample.Success {
			lastOK = sql.NullTime{Time: sample.ObservedAt.UTC(), Valid: true}
		} else {
			lastFail = sql.NullTime{Time: sample.ObservedAt.UTC(), Valid: true}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE system.service_monitor_target_states
			SET status=$2, consecutive_failures=$3, consecutive_successes=$4,
			    last_check_at=$5, last_success_at=COALESCE($6::timestamptz, last_success_at),
			    last_failure_at=COALESCE($7::timestamptz, last_failure_at), last_point_seq=$8, updated_at=$5
			WHERE target_id=$1`,
			sample.TargetID, status, fails, oks, now, lastOK, lastFail, pointSeq)
		if err != nil {
			return err
		}
		nextMonitor, err := aggregateMonitor(ctx, tx, sample.ServiceID, prevMonitor)
		if err != nil {
			return err
		}
		if err := setSourceStatus(ctx, tx, sample.ServiceID, SourceMonitor, nextMonitor, now); err != nil {
			return err
		}
		out.PointID = pointID
		out.MonitorStatus = nextMonitor
		app, err := sourceStatus(ctx, tx, sample.ServiceID, SourceApp)
		if err != nil {
			return err
		}
		out.AppStatus = app
		ep, err := lockActiveEpisode(ctx, tx, sample.ServiceID)
		if err != nil {
			return err
		}
		if ep == nil && (nextMonitor == StatusAbnormal || app == StatusAbnormal) {
			ep, err = insertEpisode(ctx, tx, sample.ServiceID, SourceMonitor, now)
			if err != nil {
				return err
			}
			out.CreatedEpisode = true
		}
		if ep == nil {
			return nil
		}
		open := nextMonitor == StatusAbnormal || (prevMonitor == StatusAbnormal && nextMonitor == StatusNormal)
		var changedIDs []string
		if open {
			proj, err := loadProjection(ctx, tx, sample.ServiceID)
			if err != nil {
				return err
			}
			ep.Projection = proj
			changedIDs = []string{pointID.String()}
		}
		if nextMonitor == StatusAbnormal {
			ep.ResolutionPending = false
			ep.FinalInputVersion = sql.NullInt64{}
		}
		if err := s.syncEpisode(ctx, tx, ep, []string{SourceMonitor}, changedIDs, nil, nextMonitor, app, now); err != nil {
			return err
		}
		out.EpisodeID = ep.ID
		out.EpisodeStatus = ep.Status
		out.InputVersion = ep.InputVersion
		return nil
	})
	return out, err
}

// AppEvent 是应用或运维人员上报的一条事件。
type AppEvent struct {
	EventID        string          `json:"event_id"`
	ProducerID     string          `json:"producer_id"`
	EventTime      time.Time       `json:"event_time"`
	SourceRevision *int64          `json:"source_revision"`
	EventKind      string          `json:"event_kind"`
	HealthStatus   string          `json:"health_status"`
	SchemaVersion  int             `json:"schema_version"`
	Payload        json.RawMessage `json:"payload"`
}

// EventResult 是上报接口的首次或重放响应。
type EventResult struct {
	Status int
	Body   map[string]any
}

// ApplyAppEvent 处理应用上报。相同 event_id 与相同内容回放首次响应。
func (s *Store) ApplyAppEvent(ctx context.Context, serviceID uuid.UUID, ev AppEvent, raw []byte) (EventResult, error) {
	var result EventResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockService(ctx, tx, serviceID); err != nil {
			return err
		}
		hash, err := RequestHash(raw)
		if err != nil {
			return errCode("VALIDATION", "payload is not valid json", 400)
		}
		var storedHash string
		var storedStatus int
		var storedBody []byte
		qerr := tx.QueryRowContext(ctx, `
			SELECT request_hash, response_status, response_body
			FROM system.application_event_receipts
			WHERE service_id=$1 AND producer_id=$2 AND event_id=$3`,
			serviceID, ev.ProducerID, ev.EventID).Scan(&storedHash, &storedStatus, &storedBody)
		if qerr == nil {
			if storedHash != hash {
				result = EventResult{Status: 409, Body: map[string]any{
					"accepted": false, "error_code": "IDEMPOTENCY_CONFLICT",
					"message": "same event_id with different content",
				}}
				return nil
			}
			body := map[string]any{}
			_ = json.Unmarshal(storedBody, &body)
			body["duplicate"] = true
			result = EventResult{Status: storedStatus, Body: body}
			return nil
		}
		if qerr != sql.ErrNoRows {
			return qerr
		}
		body, status, err := s.applyNewAppEvent(ctx, tx, serviceID, ev)
		if err != nil {
			return err
		}
		rawBody, _ := json.Marshal(body)
		var episode any
		if id, ok := body["episode_id"].(string); ok && id != "" {
			episode = id
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO system.application_event_receipts
			(receipt_id, service_id, producer_id, event_id, request_hash, episode_id, response_status, response_body, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			uuid.New(), serviceID, ev.ProducerID, ev.EventID, hash, episode, status, rawBody, s.now())
		if err != nil {
			return err
		}
		result = EventResult{Status: status, Body: body}
		return nil
	})
	return result, err
}

func (s *Store) applyNewAppEvent(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, ev AppEvent) (map[string]any, int, error) {
	if ev.EventID == "" || ev.ProducerID == "" || ev.EventTime.IsZero() || ev.SchemaVersion != 1 {
		return nil, 0, errCode("VALIDATION", "event_id, producer_id, event_time and schema_version=1 are required", 400)
	}
	if ev.EventKind != "HEALTH_STATUS" && ev.EventKind != "DIAGNOSTIC_INFO" {
		return nil, 0, errCode("VALIDATION", "event_kind must be HEALTH_STATUS or DIAGNOSTIC_INFO", 400)
	}
	if ev.EventKind == "HEALTH_STATUS" && ev.HealthStatus != StatusAbnormal && ev.HealthStatus != StatusNormal {
		return nil, 0, errCode("VALIDATION", "health_status must be ABNORMAL or NORMAL", 400)
	}
	var authority string
	if err := tx.QueryRowContext(ctx, `SELECT authority_producer_id FROM system.services WHERE service_id=$1`, serviceID).Scan(&authority); err != nil {
		return nil, 0, err
	}
	if ev.ProducerID != authority {
		return failBody("PRODUCER_NOT_AUTHORITATIVE", "producer is not the authority for this service", 403), 403, nil
	}
	now := s.now()
	if ev.EventKind == "HEALTH_STATUS" && ev.SourceRevision != nil {
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT last_source_revision FROM system.service_source_states
			WHERE service_id=$1 AND source='APP'`, serviceID).Scan(&last); err != nil {
			return nil, 0, err
		}
		if last.Valid && *ev.SourceRevision <= last.Int64 {
			return failBody("STALE_SOURCE_REVISION", "source_revision is not newer than the applied revision", 409), 409, nil
		}
	}
	monitor, err := sourceStatus(ctx, tx, serviceID, SourceMonitor)
	if err != nil {
		return nil, 0, err
	}
	app, err := sourceStatus(ctx, tx, serviceID, SourceApp)
	if err != nil {
		return nil, 0, err
	}
	ep, err := lockActiveEpisode(ctx, tx, serviceID)
	if err != nil {
		return nil, 0, err
	}
	if ev.EventKind == "DIAGNOSTIC_INFO" && ep == nil {
		return failBody("NO_ACTIVE_EPISODE", "DIAGNOSTIC_INFO requires an active episode", 409), 409, nil
	}
	if ev.EventKind == "DIAGNOSTIC_INFO" && ep != nil && ep.ResolutionPending {
		return failBody("EPISODE_CLOSING", "episode is closing and does not accept diagnostic info", 409), 409, nil
	}
	if ev.EventKind == "HEALTH_STATUS" {
		app = ev.HealthStatus
		rev := sql.NullInt64{}
		if ev.SourceRevision != nil {
			rev = sql.NullInt64{Int64: *ev.SourceRevision, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE system.service_source_states
			SET status=$2, last_event_id=$3, last_event_time=$4, last_received_at=$5,
			    last_source_revision=COALESCE($6::bigint, last_source_revision), updated_at=$5
			WHERE service_id=$1 AND source='APP'`,
			serviceID, app, ev.EventID, ev.EventTime.UTC(), now, rev); err != nil {
			return nil, 0, err
		}
	}
	if ep == nil && (monitor == StatusAbnormal || app == StatusAbnormal) {
		ep, err = insertEpisode(ctx, tx, serviceID, SourceApp, now)
		if err != nil {
			return nil, 0, err
		}
	}
	repeatNormal := ev.EventKind == "HEALTH_STATUS" && ev.HealthStatus == StatusNormal && ep != nil && ep.ResolutionPending
	var eventRecord string
	if ep != nil && !repeatNormal {
		if ev.HealthStatus == StatusAbnormal {
			ep.ResolutionPending = false
			ep.FinalInputVersion = sql.NullInt64{}
		}
		payload, err := CanonicalPayload(ev.Payload)
		if err != nil {
			return nil, 0, errCode("VALIDATION", "payload is not valid json", 400)
		}
		recordID := uuid.New()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO system.episode_app_events
			(event_record_id, episode_id, service_id, producer_id, event_id, event_kind, health_status,
			 event_time, received_at, source_revision, schema_version, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			recordID, ep.ID, serviceID, ev.ProducerID, ev.EventID, ev.EventKind, nullStr(ev.HealthStatus),
			ev.EventTime.UTC(), now, ev.SourceRevision, ev.SchemaVersion, payload)
		if err != nil {
			return nil, 0, err
		}
		eventRecord = recordID.String()
		if err := s.syncEpisode(ctx, tx, ep, []string{SourceApp}, nil, []string{eventRecord}, monitor, app, now); err != nil {
			return nil, 0, err
		}
	} else if ep != nil {
		if err := s.syncEpisode(ctx, tx, ep, nil, nil, nil, monitor, app, now); err != nil {
			return nil, 0, err
		}
	}
	body := map[string]any{
		"accepted":   true,
		"duplicate":  false,
		"event_id":   ev.EventID,
		"app_status": app,
	}
	if ep != nil {
		body["episode_id"] = ep.ID.String()
		body["episode_status"] = ep.Status
		body["input_version"] = ep.InputVersion
		body["resolution_pending"] = ep.ResolutionPending
	}
	return body, 200, nil
}

func failBody(code, message string, _ int) map[string]any {
	return map[string]any{"accepted": false, "error_code": code, "message": message}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func sourceStatus(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, source string) (string, error) {
	var status string
	err := tx.QueryRowContext(ctx, `
		SELECT status FROM system.service_source_states WHERE service_id=$1 AND source=$2`, serviceID, source).Scan(&status)
	return status, err
}

func setSourceStatus(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, source, status string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE system.service_source_states SET status=$3, updated_at=$4
		WHERE service_id=$1 AND source=$2`, serviceID, source, status, now)
	return err
}

// aggregateMonitor 按 ANY_REQUIRED：任一必需目标异常即异常，全部必需目标正常才正常，否则保持原状态。
func aggregateMonitor(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, previous string) (string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT st.status
		FROM system.service_monitor_targets t
		JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		WHERE t.service_id=$1 AND t.enabled AND t.is_required`, serviceID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	n := 0
	normal := 0
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return "", err
		}
		n++
		if st == StatusAbnormal {
			return StatusAbnormal, nil
		}
		if st == StatusNormal {
			normal++
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if n > 0 && normal == n {
		return StatusNormal, nil
	}
	if previous == "" {
		return StatusUnknown, nil
	}
	return previous, nil
}

func lockActiveEpisode(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID) (*episodeRow, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT episode_id, service_id, status, resolution_pending, started_by, started_at,
		       monitor_start_point_seq, monitor_point_watermark, current_monitor_projection,
		       input_hash, input_version, processed_input_version, final_input_version
		FROM system.episodes
		WHERE service_id=$1 AND status='ACTIVE'
		FOR UPDATE`, serviceID)
	ep, err := scanEpisode(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ep, err
}

func lockEpisode(ctx context.Context, tx *sql.Tx, episodeID uuid.UUID) (*episodeRow, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT episode_id, service_id, status, resolution_pending, started_by, started_at,
		       monitor_start_point_seq, monitor_point_watermark, current_monitor_projection,
		       input_hash, input_version, processed_input_version, final_input_version
		FROM system.episodes WHERE episode_id=$1 FOR UPDATE`, episodeID)
	ep, err := scanEpisode(row)
	if err == sql.ErrNoRows {
		return nil, errCode("EPISODE_NOT_FOUND", "episode not found", 404)
	}
	return ep, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEpisode(row rowScanner) (*episodeRow, error) {
	var ep episodeRow
	var proj []byte
	err := row.Scan(&ep.ID, &ep.ServiceID, &ep.Status, &ep.ResolutionPending, &ep.StartedBy, &ep.StartedAt,
		&ep.MonitorStart, &ep.MonitorWatermark, &proj, &ep.InputHash, &ep.InputVersion,
		&ep.ProcessedInputVersion, &ep.FinalInputVersion)
	if err != nil {
		return nil, err
	}
	if len(proj) > 0 {
		_ = json.Unmarshal(proj, &ep.Projection)
	}
	return &ep, nil
}

func insertEpisode(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, startedBy string, now time.Time) (*episodeRow, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(point_seq) FROM system.service_monitor_points WHERE service_id=$1`, serviceID).Scan(&maxSeq); err != nil {
		return nil, err
	}
	id := uuid.New()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO system.episodes
		(episode_id, service_id, status, resolution_pending, started_by, started_at,
		 monitor_start_point_seq, current_monitor_projection, input_hash, input_version,
		 processed_input_version, created_at, updated_at)
		VALUES ($1,$2,'ACTIVE',FALSE,$3,$4,$5,'[]','',0,0,$4,$4)`,
		id, serviceID, startedBy, now, maxSeq)
	if err != nil {
		return nil, err
	}
	return lockActiveEpisode(ctx, tx, serviceID)
}

func loadProjection(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID) ([]MonitorField, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.target_key, t.target_type, st.status, COALESCE((
			SELECT p.diagnostic_fields FROM system.service_monitor_points p
			WHERE p.target_id=t.target_id ORDER BY p.point_seq DESC LIMIT 1
		), '{}'::jsonb)
		FROM system.service_monitor_targets t
		JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		WHERE t.service_id=$1 AND t.enabled AND t.is_required
		ORDER BY t.target_key`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonitorField
	for rows.Next() {
		var f MonitorField
		var raw []byte
		if err := rows.Scan(&f.TargetKey, &f.TargetType, &f.Status, &raw); err != nil {
			return nil, err
		}
		var diag struct {
			StatusClass string `json:"status_class"`
			ErrorClass  string `json:"error_class"`
		}
		_ = json.Unmarshal(raw, &diag)
		if f.TargetType == "HTTP" {
			f.StatusClass = diag.StatusClass
		} else {
			f.ErrorClass = diag.ErrorClass
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func loadAppCanon(ctx context.Context, tx *sql.Tx, episodeID uuid.UUID) ([]AppCanon, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_kind, COALESCE(health_status,''), payload
		FROM system.episode_app_events WHERE episode_id=$1 ORDER BY event_seq`, episodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppCanon
	for rows.Next() {
		var ev AppCanon
		if err := rows.Scan(&ev.EventKind, &ev.HealthStatus, &ev.Payload); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

type changedRefs struct {
	MonitorPointIDs []string `json:"monitor_point_ids"`
	AppEventIDs     []string `json:"app_event_ids"`
}

func (s *Store) syncEpisode(ctx context.Context, tx *sql.Tx, ep *episodeRow, triggers []string, pointIDs, eventIDs []string, monitor, app string, now time.Time) error {
	events, err := loadAppCanon(ctx, tx, ep.ID)
	if err != nil {
		return err
	}
	hash, err := HashInput(ep.Projection, events)
	if err != nil {
		return err
	}
	var pointWM, eventWM sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(point_seq) FROM system.service_monitor_points WHERE service_id=$1`, ep.ServiceID).Scan(&pointWM); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT MAX(event_seq) FROM system.episode_app_events WHERE episode_id=$1`, ep.ID).Scan(&eventWM); err != nil {
		return err
	}
	if hash != ep.InputHash {
		ep.InputVersion++
		ep.InputHash = hash
		if len(pointIDs) > 0 && pointWM.Valid {
			ep.MonitorWatermark = pointWM
		}
		projJSON, _ := json.Marshal(ep.Projection)
		refs := changedRefs{MonitorPointIDs: pointIDs, AppEventIDs: eventIDs}
		if refs.MonitorPointIDs == nil {
			refs.MonitorPointIDs = []string{}
		}
		if refs.AppEventIDs == nil {
			refs.AppEventIDs = []string{}
		}
		refJSON, _ := json.Marshal(refs)
		if len(triggers) == 0 {
			triggers = []string{"INITIAL"}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system.episode_input_versions
			(episode_id, input_version, input_hash, monitor_projection, monitor_point_watermark,
			app_event_watermark, trigger_sources, changed_refs, queue_status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'PENDING',$9)`,
			ep.ID, ep.InputVersion, hash, projJSON, ep.MonitorWatermark, eventWM, triggers, refJSON, now); err != nil {
			return err
		}
	}
	final := ep.FinalInputVersion
	if monitor == StatusNormal && app == StatusNormal && ep.Status == EpisodeActive {
		if !ep.ResolutionPending {
			ep.ResolutionPending = true
			final = sql.NullInt64{Int64: ep.InputVersion, Valid: true}
			ep.FinalInputVersion = final
		}
	}
	projJSON, _ := json.Marshal(ep.Projection)
	_, err = tx.ExecContext(ctx, `
		UPDATE system.episodes
		SET resolution_pending=$2, current_monitor_projection=$3, input_hash=$4, input_version=$5,
		    monitor_point_watermark=$6, final_input_version=$7,
		    resolution_pending_at=CASE
		        WHEN $2 AND NOT resolution_pending THEN $8
		        WHEN NOT $2 THEN NULL
		        ELSE resolution_pending_at END,
		    updated_at=$8
		WHERE episode_id=$1`,
		ep.ID, ep.ResolutionPending, projJSON, ep.InputHash, ep.InputVersion, ep.MonitorWatermark, final, now)
	if err != nil {
		return err
	}
	if err := s.tryResolve(ctx, tx, ep, monitor, app, now); err != nil {
		return err
	}
	if ep.Status == EpisodeActive {
		return s.ensureDiagnosisTask(ctx, tx, ep, now)
	}
	return nil
}
