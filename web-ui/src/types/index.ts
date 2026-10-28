/**
 * Shared domain types. These mirror the backend's JSON shapes (to be built) and
 * the `config.yaml` schema, so the same types are reused once the API is wired.
 */

/**
 * `warning` is a check that failed and then recovered on a retry: the service
 * answered, so it counts as uptime and opens no incident. It is sticky — the
 * service stays in warning until a cycle succeeds on its first attempt.
 */
export type ServiceStatus = 'online' | 'offline' | 'warning' | 'unknown'

export type WidgetMode = 'icon' | 'name' | 'dashboard'

/** How a service's response-time history is drawn, on both the card and the detail page. */
export type ChartType = 'line' | 'bars'

export type UserRole = 'admin' | 'readonly'

/** Grid position of a service card (maps to grid-layout-plus item + config.yaml `layout`). */
export interface WidgetLayout {
  x: number
  y: number
  w: number
  h: number
}

/** Health-check configuration for a single service. */
export interface ServiceCheck {
  interval: number // seconds
  method: string
  timeout: number // seconds
  expectedStatus: number[] // empty ⇒ any 2xx is "online"
  /** Attempts per cycle before the service is called offline. 1 disables retries. */
  retryAttempts: number
  /** Waits (seconds) between attempts; the last value repeats. */
  retryDelays: number[]
}

/** A monitored service — the core domain entity. */
export interface Service {
  id: string
  name: string
  url: string
  icon: string | null
  check: ServiceCheck
  widget: { mode: WidgetMode }
  chart: { type: ChartType }
  layout: WidgetLayout
  // Runtime state (from the latest check + aggregates; mocked for now).
  status: ServiceStatus
  latencyMs: number | null
  uptime: number // 0..100 over the retention window
  errorCount: number
  /** Cycles in the window that only succeeded on a retry. */
  warningCount: number
  lastCheck: string | null // ISO timestamp
  lastSuccess: string | null // ISO timestamp
  /**
   * Recent check latencies for the sparkline, chronological. `null` means that
   * check was **offline** — not that the reading is missing.
   */
  latencyHistory: (number | null)[]
  /**
   * Parallel to `latencyHistory`. A warning check carries a latency just like an
   * online one, so the status is the only way to tell them apart.
   */
  statusHistory: ServiceStatus[]
}

/**
 * One bucketed point of the response-time / error time series.
 * `avgLatency: null` means no check succeeded in this bucket — the service was
 * down for all of it. Buckets with no checks at all are omitted entirely, so
 * consecutive points are not necessarily adjacent in time.
 */
export interface SeriesPoint {
  ts: number
  avgLatency: number | null
  errors: number
  /** Checks in this bucket that recovered on a retry. */
  warnings: number
}

/** One stored check cycle, as listed in a service's ping console. */
export interface CheckRow {
  id: number
  ts: string
  status: ServiceStatus
  latencyMs: number | null
  statusCode: number | null
  /** For a warning, why the FIRST attempt failed. */
  error: string
  attempts: number
}

/** One runtime timeline event. Times are UTC RFC3339. */
export interface TimelineEvent {
  id: number
  eventKey: string
  serviceId: string
  source: string
  eventType: string
  severity: string
  title: string
  summary: string
  occurredAt: string
  observedAt: string
  recordedAt: string
  resource?: {
    clusterId?: string
    namespace?: string
    kind?: string
    name?: string
    uid?: string
  }
  incidentId?: number
  details: Record<string, unknown>
}

export interface TimelineCoverage {
  status: string
  lastSuccessAt: string | null
  lastError?: string
}

export interface TimelinePage {
  events: TimelineEvent[]
  nextCursor: string | null
  window: { from: string; to: string; retentionDays: number }
  coverage: Record<string, TimelineCoverage>
}

/** A page of check rows; `nextBefore` is null once there is nothing older. */
export interface ChecksPage {
  checks: CheckRow[]
  nextBefore: number | null
}

/** Current TLS certificate snapshot for a service (null for HTTP services). */
export interface ServiceTls {
  checkedAt: string
  validFrom: string | null
  validUntil: string | null
  issuer: string
  subject: string
  daysLeft: number | null
  error: string
}

export interface UptimeWindows {
  days7: number
  days30: number
  days365: number
}

/** Response of GET /api/services/:id/metrics — a Service plus detail extras. */
export interface ServiceMetrics extends Service {
  series: SeriesPoint[]
  /** The requested window (unix seconds). The chart's x-domain, not the data's extent. */
  from: number
  to: number
  /** Width of one series bucket; also the gap threshold for breaking the line. */
  bucketSeconds: number
  uptimeWindows: UptimeWindows
  tls: ServiceTls | null
}

export type IncidentStatus = 'ongoing' | 'resolved'
export type IncidentSource = 'auto' | '手动'
/**
 * `outage` is a real, trackable incident (subject to the one-ongoing-per-service
 * rule). `warning` is a momentary retry-recovery event — always already
 * resolved, recorded so it shows up alongside outages in an incident feed.
 */
export type IncidentSeverity = 'outage' | 'warning'

export interface Incident {
  id: number
  serviceId: string
  serviceName: string
  status: IncidentStatus
  source: IncidentSource
  severity: IncidentSeverity
  title: string
  startedAt: string
  resolvedAt: string | null
  createdBy: number | null
  createdAt: string
  updatedAt: string
}

/** A page of incidents; `total` matches the same filter, ignoring limit/offset. */
export interface IncidentsPage {
  incidents: Incident[]
  total: number
}

export interface IncidentComment {
  id: number
  incidentId: number
  username: string
  body: string
  createdAt: string
}

export interface IncidentDetail extends Incident {
  comments: IncidentComment[]
}

export interface Delivery {
  id: number
  eventId: string
  incidentId: number | null
  integrationId: number
  integrationName: string
  integrationType: string
  eventType: string
  status: 'pending' | 'running' | 'retry_wait' | 'succeeded' | 'dead' | 'cancelled'
  attemptCount: number
  nextAttemptAt: string
  lastError: string
  sentAt: string | null
}

export type IntegrationType = 'telegram' | 'slack' | 'email' | 'webhook' | 'agent_webhook'

export interface Integration {
  id: number
  type: IntegrationType
  name: string
  enabled: boolean
  /** Opt-in to warning events (a check that recovered on a retry). Off by default. */
  notifyWarnings: boolean
  config: Record<string, unknown>
  secrets: Record<string, boolean>
  createdAt: string
  updatedAt: string
}

export interface User {
  id: number
  username: string
  role: UserRole
  createdAt: string
}

export interface AppSettings {
  defaultWidgetMode: WidgetMode
  theme: 'dark' | 'light' | 'system'
  check: {
    defaultInterval: number
    timeout: number
    retentionDays: number
    retryAttempts: number
    retryDelays: number[]
  }
  dashboard: {
    /** Look-back window (hours) for the dashboard's "警告" tile. */
    warningPeriodHours: number
    /** Service ids left out of the "平均在线率" tile's average. */
    uptimeExcludedServices: string[]
  }
  /** 诊断 Agent 与检索模型（密钥在读取时被打码；留空＝保持不变）。 */
  agent: AgentSettings
  configDir: string
}

/** 一个模型端点（OpenAI 兼容；rerank 另有 style）。 */
export interface ModelEndpointSettings {
  baseUrl: string
  /** 密钥：读取时是打码串，提交时留空/打码＝沿用已存的值。 */
  apiKey: string
  model: string
  style?: string
  dim?: number
}

export interface AgentSettings {
  baseUrl: string
  apiToken: string
  llm: ModelEndpointSettings
  embed: ModelEndpointSettings
  rerank: ModelEndpointSettings
  /** 长期记忆开关（事件簿 + 向量召回）。 */
  memoryEnabled: boolean
  /** 单次 prompt 的上下文预算（token）；0 表示用 Agent 默认值。 */
  contextBudgetTokens: number
}

/** Runtime aggregates the dashboard's stat tiles need beyond GET /api/services. */
export interface DashboardStats {
  /** Distinct services that logged a warning within `settings.dashboard.warningPeriodHours`. */
  warningServiceCount: number
}

/** 结论：一句话定性 + 归属层 + 置信度 + 依据；recurrenceOf 指向历史事件（ep:<id>）。 */
export interface AgentConclusion {
  text: string
  layer?: string
  confidence?: 'low' | 'medium' | 'high'
  evidence_ids?: string[]
  recurrence_of?: string | null
}

/** 分层假设：layer 表示原因归属（dependency/app_internal/probe/…），status 表示证据强度。 */
export interface AgentHypothesis {
  id: string
  text: string
  layer?: string
  status: string
  confidence?: string | null
  supporting_evidence_ids?: string[]
  weakening_evidence_ids?: string[]
  rationale?: string
}

/** The diagnostic agent's report for an incident, proxied by AlertFlow from the
 *  agent service (GET /api/incidents/:id/agent-report). */
export interface AgentReport {
  available: boolean
  /** Why there is no report: 'no agent run for this incident' | 'agent not configured' | 'agent unreachable'. */
  reason?: string
  runId?: string
  status?: string
  eventType?: string
  createdAt?: string
  usage?: { mode?: string; tokens?: number; llm_calls?: number; tool_calls?: number }
  report?: {
    version?: number
    summary?: string
    conclusion?: AgentConclusion
    hypotheses?: AgentHypothesis[]
    coverage?: {
      check_samples?: number
      diagnostic_source?: string
      gaps?: string[]
      from?: number
      to?: number
    }
    unknowns?: { code: string; text: string; evidence_ids?: string[] }[]
    recommended_next_steps?: { text: string; rationale?: string }[]
  }
  evidence?: Record<string, unknown>
}

/** 与 Agent 的排查会话：一轮问答。 */
export interface AgentChatMessage {
  role: 'user' | 'agent'
  text: string
  at?: string
  evidence_ids?: string[]
}

/** 一次异步排查请求。回答由 Agent 在后台产生，前端轮询这个状态。 */
export interface AgentChatRequest {
  requestId: string
  incidentId: number
  status: 'pending' | 'answered' | 'failed'
  message: string
  reply?: string
  evidenceIds?: string[]
  error?: string
  createdAt: string
  updatedAt: string
}

