package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ServiceSummary 是服务列表的读模型。
type ServiceSummary struct {
	ServiceID       uuid.UUID  `json:"service_id"`
	ServiceKey      string     `json:"service_key"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	Enabled         bool       `json:"enabled"`
	MonitorStatus   string     `json:"monitor_status"`
	AppStatus       string     `json:"app_status"`
	TargetCount     int        `json:"target_count"`
	LastCheckAt     *time.Time `json:"last_check_at"`
	ActiveEpisodeID *uuid.UUID `json:"active_episode_id"`
}

// ServiceFilter 是服务列表的过滤条件。
type ServiceFilter struct {
	Status string
	Q      string
	Limit  int
	Offset int
}

// likeFuzzy 把关键字转成 ILIKE 模式串，转义 LIKE 元字符 % 和 _；
// 反斜杠一并转义（PostgreSQL 的 LIKE 默认转义符即反斜杠）。
func likeFuzzy(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// ListServices 返回未软删除服务的摘要，status 按 MONITOR 聚合状态过滤。
func (s *Store) ListServices(ctx context.Context, f ServiceFilter) ([]ServiceSummary, int, error) {
	if f.Status != "" && f.Status != StatusNormal && f.Status != StatusAbnormal && f.Status != StatusUnknown {
		return nil, 0, errCode("VALIDATION", "status must be NORMAL, ABNORMAL or UNKNOWN", 400)
	}
	if f.Offset < 0 {
		return nil, 0, errCode("VALIDATION", "offset must not be negative", 400)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	args := []any{}
	where := "s.deleted_at IS NULL"
	if f.Status != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND m.status=$%d", len(args))
	}
	// q 对名称与 service_key 做不区分大小写的模糊匹配。
	if q := strings.TrimSpace(f.Q); q != "" {
		args = append(args, likeFuzzy(q))
		where += fmt.Sprintf(" AND (s.name ILIKE $%d OR s.service_key ILIKE $%d)", len(args), len(args))
	}
	var total int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*)
		FROM system.services s
		JOIN system.service_source_states m ON m.service_id=s.service_id AND m.source='MONITOR'
		WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	listArgs := append(append([]any{}, args...), limit, f.Offset)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT s.service_id, s.service_key, s.name, COALESCE(s.description,''), s.enabled,
		       m.status, a.status,
		       COALESCE(tc.target_count,0), lc.last_check_at, ep.episode_id
		FROM system.services s
		JOIN system.service_source_states m ON m.service_id=s.service_id AND m.source='MONITOR'
		JOIN system.service_source_states a ON a.service_id=s.service_id AND a.source='APP'
		LEFT JOIN LATERAL (
		    SELECT count(*) AS target_count
		    FROM system.service_monitor_targets t
		    WHERE t.service_id=s.service_id AND t.deleted_at IS NULL
		) tc ON TRUE
		LEFT JOIN LATERAL (
		    SELECT max(st.last_check_at) AS last_check_at
		    FROM system.service_monitor_targets t
		    JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		    WHERE t.service_id=s.service_id AND t.deleted_at IS NULL
		) lc ON TRUE
		LEFT JOIN LATERAL (
		    SELECT e.episode_id
		    FROM system.episodes e
		    WHERE e.service_id=s.service_id AND e.status='ACTIVE'
		    ORDER BY e.started_at DESC LIMIT 1
		) ep ON TRUE
		WHERE `+where+`
		ORDER BY s.service_key, s.service_id
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ServiceSummary
	for rows.Next() {
		var v ServiceSummary
		var lastCheck sql.NullTime
		var episodeID uuid.NullUUID
		if err := rows.Scan(&v.ServiceID, &v.ServiceKey, &v.Name, &v.Description, &v.Enabled,
			&v.MonitorStatus, &v.AppStatus, &v.TargetCount, &lastCheck, &episodeID); err != nil {
			return nil, 0, err
		}
		if lastCheck.Valid {
			v.LastCheckAt = &lastCheck.Time
		}
		if episodeID.Valid {
			v.ActiveEpisodeID = &episodeID.UUID
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// SoftDeleteService 软删除服务并停用其未删除目标，历史数据保留。
func (s *Store) SoftDeleteService(ctx context.Context, id uuid.UUID) error {
	now := s.now()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE system.services
			SET deleted_at=$2, enabled=FALSE, updated_at=$2
			WHERE service_id=$1 AND deleted_at IS NULL`, id, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errCode("SERVICE_NOT_FOUND", "service not found", 404)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE system.service_monitor_targets
			SET enabled=FALSE, updated_at=$2
			WHERE service_id=$1 AND deleted_at IS NULL`, id, now)
		return err
	})
}

// CreateTarget 在指定服务下新增监控目标。
func (s *Store) CreateTarget(ctx context.Context, serviceID uuid.UUID, in TargetInput) (Target, error) {
	var tid uuid.UUID
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockService(ctx, tx, serviceID); err != nil {
			return err
		}
		id, err := insertTarget(ctx, tx, serviceID, in, s.now())
		if err != nil {
			return err
		}
		tid = id
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	return s.getTarget(ctx, tid)
}

// PatchTargetInput 是目标的部分更新，字段为 nil 表示不修改。
type PatchTargetInput struct {
	TargetName        *string          `json:"target_name"`
	Address           *string          `json:"address"`
	Config            *json.RawMessage `json:"config"`
	IntervalMS        *int             `json:"interval_ms"`
	TimeoutMS         *int             `json:"timeout_ms"`
	FailureThreshold  *int             `json:"failure_threshold"`
	RecoveryThreshold *int             `json:"recovery_threshold"`
	Required          *bool            `json:"is_required"`
	Enabled           *bool            `json:"enabled"`
}

// UpdateTarget 合并部分更新后按目标类型重新校验，返回更新后的目标与所属服务。
func (s *Store) UpdateTarget(ctx context.Context, targetID uuid.UUID, in PatchTargetInput) (Target, uuid.UUID, error) {
	var serviceID uuid.UUID
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, sid, err := lockTarget(ctx, tx, targetID)
		if err != nil {
			return err
		}
		serviceID = sid
		merged := TargetInput{
			TargetKey:         cur.TargetKey,
			TargetName:        cur.TargetName,
			TargetType:        cur.TargetType,
			Address:           cur.Address,
			Config:            cur.Config,
			IntervalMS:        cur.IntervalMS,
			TimeoutMS:         cur.TimeoutMS,
			FailureThreshold:  cur.FailureThreshold,
			RecoveryThreshold: cur.RecoveryThreshold,
			Required:          &cur.Required,
			Enabled:           &cur.Enabled,
		}
		if in.TargetName != nil {
			merged.TargetName = *in.TargetName
		}
		if in.Address != nil {
			merged.Address = *in.Address
		}
		if in.Config != nil {
			merged.Config = *in.Config
		}
		if in.IntervalMS != nil {
			merged.IntervalMS = *in.IntervalMS
		}
		if in.TimeoutMS != nil {
			merged.TimeoutMS = *in.TimeoutMS
		}
		if in.FailureThreshold != nil {
			merged.FailureThreshold = *in.FailureThreshold
		}
		if in.RecoveryThreshold != nil {
			merged.RecoveryThreshold = *in.RecoveryThreshold
		}
		if in.Required != nil {
			merged.Required = in.Required
		}
		if in.Enabled != nil {
			merged.Enabled = in.Enabled
		}
		t, err := normalizeTarget(merged)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE system.service_monitor_targets
			SET target_name=$2, address=$3, config=$4, interval_ms=$5, timeout_ms=$6,
			    failure_threshold=$7, recovery_threshold=$8, is_required=$9, enabled=$10, updated_at=$11
			WHERE target_id=$1 AND deleted_at IS NULL`,
			targetID, t.TargetName, t.Address, t.Config, t.IntervalMS, t.TimeoutMS,
			t.FailureThreshold, t.RecoveryThreshold, reqValue(t.Required), reqValue(t.Enabled), s.now())
		return err
	})
	if err != nil {
		return Target{}, uuid.Nil, err
	}
	updated, err := s.getTarget(ctx, targetID)
	return updated, serviceID, err
}

func reqValue(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// SoftDeleteTarget 软删除目标并停用，允许之后用同一 target_key 重建。
func (s *Store) SoftDeleteTarget(ctx context.Context, targetID uuid.UUID) (uuid.UUID, error) {
	now := s.now()
	var serviceID uuid.UUID
	err := s.DB.QueryRowContext(ctx, `
		UPDATE system.service_monitor_targets
		SET deleted_at=$2, enabled=FALSE, updated_at=$2
		WHERE target_id=$1 AND deleted_at IS NULL
		RETURNING service_id`, targetID, now).Scan(&serviceID)
	if err == sql.ErrNoRows {
		return uuid.Nil, errCode("TARGET_NOT_FOUND", "target not found", 404)
	}
	return serviceID, err
}

// lockTarget 锁定未删除目标并校验所属服务未软删除。
func lockTarget(ctx context.Context, tx *sql.Tx, targetID uuid.UUID) (Target, uuid.UUID, error) {
	var t Target
	var serviceID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT t.service_id, t.target_key, t.target_name, t.target_type, t.address, t.config,
		       t.interval_ms, t.timeout_ms, t.failure_threshold, t.recovery_threshold,
		       t.is_required, t.enabled
		FROM system.service_monitor_targets t
		JOIN system.services s ON s.service_id=t.service_id
		WHERE t.target_id=$1 AND t.deleted_at IS NULL AND s.deleted_at IS NULL
		FOR UPDATE OF t`, targetID).Scan(
		&serviceID, &t.TargetKey, &t.TargetName, &t.TargetType, &t.Address, &t.Config,
		&t.IntervalMS, &t.TimeoutMS, &t.FailureThreshold, &t.RecoveryThreshold, &t.Required, &t.Enabled)
	if err == sql.ErrNoRows {
		return Target{}, uuid.Nil, errCode("TARGET_NOT_FOUND", "target not found", 404)
	}
	if err != nil {
		return Target{}, uuid.Nil, err
	}
	t.TargetID = targetID
	return t, serviceID, nil
}

// getTarget 按 ID 返回未删除目标及其状态。
func (s *Store) getTarget(ctx context.Context, targetID uuid.UUID) (Target, error) {
	var t Target
	var lastCheck sql.NullTime
	err := s.DB.QueryRowContext(ctx, `
		SELECT t.target_id, t.target_key, t.target_name, t.target_type, t.address, t.config,
		       t.interval_ms, t.timeout_ms, t.failure_threshold, t.recovery_threshold,
		       t.is_required, t.enabled, st.status, st.consecutive_failures, st.consecutive_successes,
		       st.last_check_at
		FROM system.service_monitor_targets t
		JOIN system.services s ON s.service_id=t.service_id
		JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		WHERE t.target_id=$1 AND t.deleted_at IS NULL AND s.deleted_at IS NULL`, targetID).Scan(
		&t.TargetID, &t.TargetKey, &t.TargetName, &t.TargetType, &t.Address, &t.Config,
		&t.IntervalMS, &t.TimeoutMS, &t.FailureThreshold, &t.RecoveryThreshold,
		&t.Required, &t.Enabled, &t.Status, &t.ConsecutiveFailures, &t.ConsecutiveSuccesses, &lastCheck)
	if err == sql.ErrNoRows {
		return Target{}, errCode("TARGET_NOT_FOUND", "target not found", 404)
	}
	if err != nil {
		return Target{}, err
	}
	if lastCheck.Valid {
		t.LastCheckAt = &lastCheck.Time
	}
	return t, nil
}

// EpisodeSummary 是 Episode 列表的读模型。
type EpisodeSummary struct {
	EpisodeID             uuid.UUID  `json:"episode_id"`
	ServiceID             uuid.UUID  `json:"service_id"`
	ServiceName           string     `json:"service_name"`
	Status                string     `json:"status"`
	ResolutionPending     bool       `json:"resolution_pending"`
	StartedBy             string     `json:"started_by"`
	StartedAt             time.Time  `json:"started_at"`
	ResolvedAt            *time.Time `json:"resolved_at"`
	ArchivedAt            *time.Time `json:"archived_at"`
	InputVersion          int64      `json:"input_version"`
	ProcessedInputVersion int64      `json:"processed_input_version"`
	ActiveTaskStatus      string     `json:"active_task_status"`
	ActiveTaskType        string     `json:"active_task_type"`
	HasDiagnosisReport    bool       `json:"has_diagnosis_report"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// EpisodeFilter 是 Episode 列表的过滤条件。
type EpisodeFilter struct {
	Status    string
	ServiceID *uuid.UUID
	From      *time.Time
	To        *time.Time
	Limit     int
	Offset    int
}

// ListEpisodes 返回 Episode 摘要，包含已禁用或软删服务的历史 Episode。
func (s *Store) ListEpisodes(ctx context.Context, f EpisodeFilter) ([]EpisodeSummary, int, error) {
	if f.Status != "" && f.Status != EpisodeActive && f.Status != EpisodeResolved && f.Status != EpisodeArchived {
		return nil, 0, errCode("VALIDATION", "status must be ACTIVE, RESOLVED or ARCHIVED", 400)
	}
	if f.Offset < 0 {
		return nil, 0, errCode("VALIDATION", "offset must not be negative", 400)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	args := []any{}
	where := "TRUE"
	if f.Status != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND e.status=$%d", len(args))
	}
	if f.ServiceID != nil {
		args = append(args, *f.ServiceID)
		where += fmt.Sprintf(" AND e.service_id=$%d", len(args))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where += fmt.Sprintf(" AND e.started_at >= $%d", len(args))
	}
	if f.To != nil {
		args = append(args, *f.To)
		where += fmt.Sprintf(" AND e.started_at < $%d", len(args))
	}
	var total int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM system.episodes e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	listArgs := append(append([]any{}, args...), limit, f.Offset)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.episode_id, e.service_id, sv.name, e.status, e.resolution_pending, e.started_by,
		       e.started_at, e.resolved_at, e.archived_at, e.input_version, e.processed_input_version,
		       COALESCE(t.status,''), COALESCE(t.task_type,''),
		       EXISTS (SELECT 1 FROM cognitive.episode_artifacts a
		               WHERE a.episode_id=e.episode_id AND a.artifact_type='DIAGNOSIS_REPORT'),
		       e.updated_at
		FROM system.episodes e
		JOIN system.services sv ON sv.service_id=e.service_id
		LEFT JOIN system.agent_tasks t ON t.episode_id=e.episode_id
		  AND t.status IN ('PENDING','DISPATCHED','RUNNING','RETRY_WAIT')
		WHERE `+where+`
		ORDER BY e.started_at DESC, e.episode_id DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []EpisodeSummary
	for rows.Next() {
		var v EpisodeSummary
		var resolved, archived sql.NullTime
		if err := rows.Scan(&v.EpisodeID, &v.ServiceID, &v.ServiceName, &v.Status, &v.ResolutionPending,
			&v.StartedBy, &v.StartedAt, &resolved, &archived, &v.InputVersion, &v.ProcessedInputVersion,
			&v.ActiveTaskStatus, &v.ActiveTaskType, &v.HasDiagnosisReport, &v.UpdatedAt); err != nil {
			return nil, 0, err
		}
		if resolved.Valid {
			v.ResolvedAt = &resolved.Time
		}
		if archived.Valid {
			v.ArchivedAt = &archived.Time
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// TrendBucket 是 5 分钟粒度的监控成功/失败计数。
type TrendBucket struct {
	BucketStart time.Time `json:"bucket_start"`
	Success     int64     `json:"success"`
	Failure     int64     `json:"failure"`
}

// DashboardSummary 是控制台首页的一次性聚合结果。
type DashboardSummary struct {
	GeneratedAt    time.Time        `json:"generated_at"`
	Services       ServiceCounts    `json:"services"`
	Targets        TargetCounts     `json:"targets"`
	Episodes       EpisodeCounts    `json:"episodes"`
	RecentEpisodes []EpisodeSummary `json:"recent_episodes"`
	MonitorTrend   []TrendBucket    `json:"monitor_trend"`
}

type ServiceCounts struct {
	Total    int `json:"total"`
	Normal   int `json:"normal"`
	Abnormal int `json:"abnormal"`
	Unknown  int `json:"unknown"`
	Disabled int `json:"disabled"`
}

type TargetCounts struct {
	Total    int `json:"total"`
	Normal   int `json:"normal"`
	Abnormal int `json:"abnormal"`
	Unknown  int `json:"unknown"`
}

type EpisodeCounts struct {
	Active   int `json:"active"`
	Resolved int `json:"resolved"`
	Archived int `json:"archived"`
}

// Dashboard 返回控制台首页聚合数据，排除软删除的服务与目标。
func (s *Store) Dashboard(ctx context.Context) (DashboardSummary, error) {
	now := s.now()
	var out DashboardSummary
	out.GeneratedAt = now
	err := s.DB.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE m.status='NORMAL'),
		       count(*) FILTER (WHERE m.status='ABNORMAL'),
		       count(*) FILTER (WHERE m.status='UNKNOWN'),
		       count(*) FILTER (WHERE NOT s.enabled)
		FROM system.services s
		JOIN system.service_source_states m ON m.service_id=s.service_id AND m.source='MONITOR'
		WHERE s.deleted_at IS NULL`).Scan(
		&out.Services.Total, &out.Services.Normal, &out.Services.Abnormal,
		&out.Services.Unknown, &out.Services.Disabled)
	if err != nil {
		return DashboardSummary{}, err
	}
	err = s.DB.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE st.status='NORMAL'),
		       count(*) FILTER (WHERE st.status='ABNORMAL'),
		       count(*) FILTER (WHERE st.status='UNKNOWN')
		FROM system.service_monitor_targets t
		JOIN system.services s ON s.service_id=t.service_id
		JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		WHERE t.deleted_at IS NULL AND s.deleted_at IS NULL`).Scan(
		&out.Targets.Total, &out.Targets.Normal, &out.Targets.Abnormal, &out.Targets.Unknown)
	if err != nil {
		return DashboardSummary{}, err
	}
	err = s.DB.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE status='ACTIVE'),
		       count(*) FILTER (WHERE status='RESOLVED'),
		       count(*) FILTER (WHERE status='ARCHIVED')
		FROM system.episodes`).Scan(
		&out.Episodes.Active, &out.Episodes.Resolved, &out.Episodes.Archived)
	if err != nil {
		return DashboardSummary{}, err
	}
	recent, _, err := s.ListEpisodes(ctx, EpisodeFilter{Limit: 10})
	if err != nil {
		return DashboardSummary{}, err
	}
	out.RecentEpisodes = recent
	if out.RecentEpisodes == nil {
		out.RecentEpisodes = []EpisodeSummary{}
	}
	trend, err := s.monitorTrend(ctx, now)
	if err != nil {
		return DashboardSummary{}, err
	}
	out.MonitorTrend = trend
	return out, nil
}

// monitorTrend 按 5 分钟 epoch 对齐返回最近 6 个桶；窗口内无点返回空数组。
func (s *Store) monitorTrend(ctx context.Context, now time.Time) ([]TrendBucket, error) {
	const bucket = 5 * time.Minute
	end := now.Truncate(bucket).Add(bucket)
	start := end.Add(-6 * bucket)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT to_timestamp(floor(extract(epoch FROM observed_at)/300)*300) AS bucket_start,
		       count(*) FILTER (WHERE check_status IN ('NORMAL','SUCCESS')),
		       count(*) FILTER (WHERE check_status NOT IN ('NORMAL','SUCCESS'))
		FROM system.service_monitor_points
		WHERE observed_at >= $1 AND observed_at < $2
		GROUP BY 1 ORDER BY 1`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[int64]TrendBucket{}
	var total int64
	for rows.Next() {
		var b TrendBucket
		if err := rows.Scan(&b.BucketStart, &b.Success, &b.Failure); err != nil {
			return nil, err
		}
		counts[b.BucketStart.Unix()] = b
		total += b.Success + b.Failure
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return []TrendBucket{}, nil
	}
	out := make([]TrendBucket, 0, 6)
	for i := 0; i < 6; i++ {
		ts := start.Add(time.Duration(i) * bucket)
		b := counts[ts.Unix()]
		b.BucketStart = ts
		out = append(out, b)
	}
	return out, nil
}
