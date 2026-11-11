package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	StatusUnknown  = "UNKNOWN"
	StatusNormal   = "NORMAL"
	StatusAbnormal = "ABNORMAL"

	SourceMonitor = "MONITOR"
	SourceApp     = "APP"

	EpisodeActive   = "ACTIVE"
	EpisodeResolved = "RESOLVED"
	EpisodeArchived = "ARCHIVED"

	TaskPending    = "PENDING"
	TaskDispatched = "DISPATCHED"
	TaskRunning    = "RUNNING"
	TaskRetryWait  = "RETRY_WAIT"
	TaskSucceeded  = "SUCCEEDED"
	TaskFailed     = "FAILED"

	TaskDiagnosis = "DIAGNOSIS"
	TaskArchive   = "ARCHIVE"
)

// Store 是系统侧事务入口。同一 Service 的写先锁 Service 行。
type Store struct {
	DB          *sql.DB
	Now         func() time.Time
	MaxAttempts int
	Lease       time.Duration
	RetryBase   time.Duration
}

func New(db *sql.DB) *Store {
	return &Store{
		DB:          db,
		Now:         func() time.Time { return time.Now().UTC() },
		MaxAttempts: 3,
		Lease:       30 * time.Second,
		RetryBase:   time.Second,
	}
}

func (s *Store) now() time.Time { return s.Now().UTC() }

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TargetInput 是创建服务时的监控目标。
type TargetInput struct {
	TargetKey         string          `json:"target_key"`
	TargetName        string          `json:"target_name"`
	TargetType        string          `json:"target_type"`
	Address           string          `json:"address"`
	Config            json.RawMessage `json:"config"`
	IntervalMS        int             `json:"interval_ms"`
	TimeoutMS         int             `json:"timeout_ms"`
	FailureThreshold  int             `json:"failure_threshold"`
	RecoveryThreshold int             `json:"recovery_threshold"`
	Required          *bool           `json:"is_required"`
	Enabled           *bool           `json:"enabled"`
}

// CreateServiceInput 创建 Service，并初始化 MONITOR 与 APP 两条来源。
type CreateServiceInput struct {
	ServiceKey          string        `json:"service_key"`
	Name                string        `json:"name"`
	Description         string        `json:"description"`
	AuthorityProducerID string        `json:"authority_producer_id"`
	Targets             []TargetInput `json:"targets"`
}

// Target 是已保存的监控目标。
type Target struct {
	TargetID             uuid.UUID       `json:"target_id"`
	TargetKey            string          `json:"target_key"`
	TargetName           string          `json:"target_name"`
	TargetType           string          `json:"target_type"`
	Address              string          `json:"address"`
	Config               json.RawMessage `json:"config"`
	IntervalMS           int             `json:"interval_ms"`
	TimeoutMS            int             `json:"timeout_ms"`
	FailureThreshold     int             `json:"failure_threshold"`
	RecoveryThreshold    int             `json:"recovery_threshold"`
	Required             bool            `json:"is_required"`
	Enabled              bool            `json:"enabled"`
	Status               string          `json:"status"`
	ConsecutiveFailures  int             `json:"consecutive_failures"`
	ConsecutiveSuccesses int             `json:"consecutive_successes"`
	LastCheckAt          *time.Time      `json:"last_check_at"`
}

// KubernetesTargetConfig 是 KUBERNETES 类型目标的 config 内容。
type KubernetesTargetConfig struct {
	ResourceKind string `json:"resource_kind"`
	ResourceName string `json:"resource_name"`
}

// ParseKubernetesTargetConfig 解析并校验 KUBERNETES 目标配置。
func ParseKubernetesTargetConfig(raw json.RawMessage) (KubernetesTargetConfig, error) {
	var cfg KubernetesTargetConfig
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, errCode("VALIDATION", "config must be a valid JSON object", 400)
		}
	}
	if cfg.ResourceKind != "Deployment" && cfg.ResourceKind != "StatefulSet" {
		return cfg, errCode("VALIDATION", "config.resource_kind must be Deployment or StatefulSet", 400)
	}
	cfg.ResourceName = strings.TrimSpace(cfg.ResourceName)
	if cfg.ResourceName == "" {
		return cfg, errCode("VALIDATION", "config.resource_name is required", 400)
	}
	return cfg, nil
}

// ServiceView 是管理查询返回的服务与双来源状态。
type ServiceView struct {
	ServiceID           uuid.UUID `json:"service_id"`
	ServiceKey          string    `json:"service_key"`
	Name                string    `json:"name"`
	Description         string    `json:"description"`
	Enabled             bool      `json:"enabled"`
	AggregationMode     string    `json:"monitor_aggregation_mode"`
	AuthorityProducerID string    `json:"authority_producer_id"`
	MonitorStatus       string    `json:"monitor_status"`
	AppStatus           string    `json:"app_status"`
	Targets             []Target  `json:"targets"`
}

func (s *Store) CreateService(ctx context.Context, in CreateServiceInput) (ServiceView, error) {
	if in.ServiceKey == "" || in.Name == "" || in.AuthorityProducerID == "" {
		return ServiceView{}, errCode("VALIDATION", "service_key, name and authority_producer_id are required", 400)
	}
	if len(in.Targets) == 0 {
		return ServiceView{}, errCode("VALIDATION", "at least one monitor target is required", 400)
	}
	id := uuid.New()
	now := s.now()
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO system.services
			(service_id, service_key, name, description, enabled, monitor_aggregation_mode, authority_producer_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,TRUE,'ANY_REQUIRED',$5,$6,$6)`,
			id, in.ServiceKey, in.Name, in.Description, in.AuthorityProducerID, now)
		if err != nil {
			return err
		}
		for _, src := range []string{SourceMonitor, SourceApp} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO system.service_source_states (service_id, source, status, updated_at)
				VALUES ($1,$2,'UNKNOWN',$3)`, id, src, now); err != nil {
				return err
			}
		}
		for _, t := range in.Targets {
			if _, err := insertTarget(ctx, tx, id, t, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ServiceView{}, err
	}
	return s.GetService(ctx, id)
}

// normalizeTarget 校验目标定义，并为 KUBERNETES 类型补齐默认值。
// HTTP/TCP 保持原有校验规则。
func normalizeTarget(t TargetInput) (TargetInput, error) {
	if t.TargetKey == "" {
		return t, errCode("VALIDATION", "target_key is required", 400)
	}
	switch t.TargetType {
	case "HTTP", "TCP":
		if t.Address == "" {
			return t, errCode("VALIDATION", "address is required for HTTP|TCP target", 400)
		}
		if t.IntervalMS <= 0 || t.TimeoutMS <= 0 {
			return t, errCode("VALIDATION", "interval_ms and timeout_ms must be positive", 400)
		}
	case "KUBERNETES":
		// K8s 目标通过集群内资源名定位，address 保留空字符串。
		if t.Address != "" {
			return t, errCode("VALIDATION", "address must be empty for KUBERNETES target", 400)
		}
		if _, err := ParseKubernetesTargetConfig(t.Config); err != nil {
			return t, err
		}
		if t.IntervalMS == 0 {
			t.IntervalMS = 10000
		}
		if t.TimeoutMS == 0 {
			t.TimeoutMS = 3000
		}
	default:
		return t, errCode("VALIDATION", "target_type must be HTTP, TCP or KUBERNETES", 400)
	}
	if t.FailureThreshold <= 0 || t.RecoveryThreshold <= 0 {
		return t, errCode("VALIDATION", "failure_threshold and recovery_threshold must be positive", 400)
	}
	if len(t.Config) == 0 {
		t.Config = json.RawMessage(`{}`)
	}
	return t, nil
}

func insertTarget(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, in TargetInput, now time.Time) (uuid.UUID, error) {
	t, err := normalizeTarget(in)
	if err != nil {
		return uuid.Nil, err
	}
	required, enabled := true, true
	if t.Required != nil {
		required = *t.Required
	}
	if t.Enabled != nil {
		enabled = *t.Enabled
	}
	tid := uuid.New()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO system.service_monitor_targets
		(target_id, service_id, target_key, target_name, target_type, address, config, interval_ms, timeout_ms,
		 failure_threshold, recovery_threshold, is_required, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)`,
		tid, serviceID, t.TargetKey, t.TargetName, t.TargetType, t.Address, t.Config, t.IntervalMS, t.TimeoutMS,
		t.FailureThreshold, t.RecoveryThreshold, required, enabled, now)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO system.service_monitor_target_states
		(target_id, status, consecutive_failures, consecutive_successes, updated_at)
		VALUES ($1,'UNKNOWN',0,0,$2)`, tid, now)
	if err != nil {
		return uuid.Nil, err
	}
	return tid, nil
}

func (s *Store) GetService(ctx context.Context, id uuid.UUID) (ServiceView, error) {
	var v ServiceView
	err := s.DB.QueryRowContext(ctx, `
		SELECT s.service_id, s.service_key, s.name, COALESCE(s.description,''), s.enabled,
		       s.monitor_aggregation_mode, s.authority_producer_id,
		       m.status, a.status
		FROM system.services s
		JOIN system.service_source_states m ON m.service_id=s.service_id AND m.source='MONITOR'
		JOIN system.service_source_states a ON a.service_id=s.service_id AND a.source='APP'
		WHERE s.service_id=$1 AND s.deleted_at IS NULL`, id).Scan(
		&v.ServiceID, &v.ServiceKey, &v.Name, &v.Description, &v.Enabled, &v.AggregationMode,
		&v.AuthorityProducerID, &v.MonitorStatus, &v.AppStatus)
	if err == sql.ErrNoRows {
		return ServiceView{}, errCode("SERVICE_NOT_FOUND", "service not found", 404)
	}
	if err != nil {
		return ServiceView{}, err
	}
	v.Targets, err = s.listTargets(ctx, id)
	if err != nil {
		return ServiceView{}, err
	}
	return v, nil
}

// GetTargets 返回服务下未软删除的监控目标。
func (s *Store) GetTargets(ctx context.Context, serviceID uuid.UUID) ([]Target, error) {
	if err := s.requireService(ctx, serviceID); err != nil {
		return nil, err
	}
	return s.listTargets(ctx, serviceID)
}

func (s *Store) requireService(ctx context.Context, serviceID uuid.UUID) error {
	var got uuid.UUID
	err := s.DB.QueryRowContext(ctx,
		`SELECT service_id FROM system.services WHERE service_id=$1 AND deleted_at IS NULL`, serviceID).Scan(&got)
	if err == sql.ErrNoRows {
		return errCode("SERVICE_NOT_FOUND", "service not found", 404)
	}
	return err
}

func (s *Store) listTargets(ctx context.Context, serviceID uuid.UUID) ([]Target, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.target_id, t.target_key, t.target_name, t.target_type, t.address, t.config,
		       t.interval_ms, t.timeout_ms, t.failure_threshold, t.recovery_threshold,
		       t.is_required, t.enabled, st.status, st.consecutive_failures, st.consecutive_successes,
		       st.last_check_at
		FROM system.service_monitor_targets t
		JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		WHERE t.service_id=$1 AND t.deleted_at IS NULL
		ORDER BY t.target_key`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		var lastCheck sql.NullTime
		if err := rows.Scan(&t.TargetID, &t.TargetKey, &t.TargetName, &t.TargetType, &t.Address, &t.Config,
			&t.IntervalMS, &t.TimeoutMS, &t.FailureThreshold, &t.RecoveryThreshold,
			&t.Required, &t.Enabled, &t.Status, &t.ConsecutiveFailures, &t.ConsecutiveSuccesses,
			&lastCheck); err != nil {
			return nil, err
		}
		if lastCheck.Valid {
			t.LastCheckAt = &lastCheck.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PatchServiceInput 只改服务元数据，不改已经写入的时间轴。
type PatchServiceInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
}

func (s *Store) PatchService(ctx context.Context, id uuid.UUID, in PatchServiceInput) (ServiceView, error) {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockService(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE system.services
			SET name = COALESCE($2::varchar, name),
			    description = COALESCE($3::text, description),
			    enabled = COALESCE($4::boolean, enabled),
			    updated_at = $5
			WHERE service_id=$1`, id, in.Name, in.Description, in.Enabled, s.now())
		return err
	})
	if err != nil {
		return ServiceView{}, err
	}
	return s.GetService(ctx, id)
}

func lockService(ctx context.Context, tx *sql.Tx, id uuid.UUID) error {
	var got uuid.UUID
	err := tx.QueryRowContext(ctx,
		`SELECT service_id FROM system.services WHERE service_id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&got)
	if err == sql.ErrNoRows {
		return errCode("SERVICE_NOT_FOUND", "service not found", 404)
	}
	return err
}
