// 与 /tmp/alertflow-delivery/contracts.md 及 backend/internal/system 模型对齐的 API 类型定义。
// 字段名一律按后端 JSON（snake_case），前端不做二次命名转换。

export type MonitorStatus = 'NORMAL' | 'ABNORMAL' | 'UNKNOWN'
export type EpisodeStatus = 'ACTIVE' | 'RESOLVED' | 'ARCHIVED'
export type TargetType = 'HTTP' | 'TCP' | 'KUBERNETES'
export type ResourceKind = 'Deployment' | 'StatefulSet'

export interface ApiErrorBody {
  error_code?: string
  message?: string
}

export interface Paged {
  total: number
  limit: number
  offset: number
  // 列表字段名随接口不同，由具体接口类型补充
}

export interface DashboardCounts {
  total: number
  normal: number
  abnormal: number
  unknown: number
  disabled?: number
}

export interface DashboardEpisodeCounts {
  active: number
  resolved: number
  archived: number
}

export interface MonitorTrendPoint {
  bucket_start: string
  success: number
  failure: number
}

export interface DashboardSummary {
  generated_at: string
  services: Required<DashboardCounts>
  targets: DashboardCounts
  episodes: DashboardEpisodeCounts
  recent_episodes: EpisodeSummary[]
  monitor_trend: MonitorTrendPoint[]
}

export interface TargetConfig {
  resource_kind?: ResourceKind | string
  resource_name?: string
  [key: string]: unknown
}

export interface Target {
  target_id: string
  target_key: string
  target_name: string
  target_type: TargetType
  address: string
  config: TargetConfig | null
  interval_ms: number
  timeout_ms: number
  failure_threshold: number
  recovery_threshold: number
  is_required: boolean
  enabled: boolean
  status: MonitorStatus
  consecutive_failures: number
  consecutive_successes: number
  last_check_at?: string | null
}

/** 新增/编辑目标时提交的字段（不包含后端生成字段） */
export interface TargetInput {
  target_key?: string
  target_name: string
  target_type?: TargetType
  address: string
  config: TargetConfig
  interval_ms: number
  timeout_ms: number
  failure_threshold: number
  recovery_threshold: number
  is_required: boolean
  enabled: boolean
}

export interface ServiceSummary {
  service_id: string
  service_key: string
  name: string
  description: string
  enabled: boolean
  monitor_status: MonitorStatus
  app_status: MonitorStatus
  target_count: number
  last_check_at: string | null
  active_episode_id: string | null
}

export interface ServiceView {
  service_id: string
  service_key: string
  name: string
  description: string
  enabled: boolean
  monitor_aggregation_mode: string
  authority_producer_id: string
  monitor_status: MonitorStatus
  app_status: MonitorStatus
  targets: Target[]
  target_count?: number
  last_check_at?: string | null
  active_episode_id?: string | null
}

export interface ServicesPage extends Paged {
  services: ServiceSummary[]
}

export interface ServiceCreatePayload {
  service_key: string
  name: string
  description: string
  authority_producer_id: string
  targets: TargetInput[]
}

export interface ServicePatchPayload {
  name?: string
  description?: string
  enabled?: boolean
}

export interface EpisodeSummary {
  episode_id: string
  service_id: string
  service_name: string
  status: EpisodeStatus
  resolution_pending: boolean
  started_by: string
  started_at: string
  resolved_at: string | null
  archived_at: string | null
  input_version: number
  processed_input_version: number
  active_task_status: string
  updated_at: string
}

export interface EpisodeView {
  episode_id: string
  service_id: string
  service_name?: string
  status: EpisodeStatus
  resolution_pending: boolean
  started_by: string
  started_at: string
  resolved_at: string | null
  archived_at: string | null
  input_hash: string
  input_version: number
  processed_input_version: number
  final_input_version: number
  monitor_status: MonitorStatus
  app_status: MonitorStatus
  active_task_id: string
  active_task_status: string
  active_task_type: string
  updated_at?: string
}

export interface EpisodesPage extends Paged {
  episodes: EpisodeSummary[]
}

/** 时间轴区域一：监控投影（当前输入版本下的目标状态快照） */
export interface MonitorField {
  target_key: string
  target_type: TargetType
  status: MonitorStatus
  status_class: string
  error_class: string
  state_fingerprint?: string
}

/** 时间轴区域二：应用事件 */
export interface AppEvent {
  event_record_id: string
  event_id: string
  event_kind: string
  health_status: string
  event_time: string
  received_at: string
  payload: unknown
}

/** 时间轴区域三：产物引用 */
export interface ArtifactRef {
  artifact_type: string
  artifact_version: number
  storage_key: string
  content_type: string
  content_hash: string
  updated_at: string
}

export interface EpisodeTimeline {
  episode: EpisodeView
  region1_projection: MonitorField[] | null
  monitor_start_point_seq: number
  region2_events: AppEvent[] | null
  region3_refs: ArtifactRef[] | null
}

export type ArtifactType = 'INVESTIGATION' | 'DIAGNOSIS_REPORT'

export interface EpisodeArtifact {
  artifact_type: ArtifactType | string
  version: number | string
  content_type: string
  content_text: string
  content_json: Record<string, unknown> | null
  content_hash: string
}

/** 诊断报告结构化内容（agent schemas.DiagnosisReport） */
export interface DiagnosisReport {
  summary?: string
  observations?: string[]
  hypotheses?: string[]
  unknowns?: string[]
  next_steps?: string[]
  [key: string]: unknown
}
