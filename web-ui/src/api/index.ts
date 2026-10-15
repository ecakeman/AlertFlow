import { request, requestRaw, getBlob } from './http'
import type {
  Service,
  ServiceMetrics,
  ChecksPage,
  Incident,
  IncidentSeverity,
  IncidentsPage,
  IncidentComment,
  Delivery,
  IncidentDetail,
  AgentReport,
  AgentChatRequest,
  Integration,
  IntegrationType,
  User,
  UserRole,
  AppSettings,
  AgentSettings,
  DashboardStats,
  WidgetMode,
  ChartType,
} from '@/types'

export type MetricsRange = '1h' | '6h' | '24h' | '7d' | '30d'

export interface IncidentInput {
  serviceId?: string
  title?: string
  startedAt?: string | null
  resolvedAt?: string | null
}

export interface IntegrationInput {
  type: IntegrationType
  name: string
  enabled: boolean
  /** Always send the current value: unlike a secret, a blank bool cannot mean "keep". */
  notifyWarnings: boolean
  config: Record<string, unknown>
}

export { ApiError } from './http'

export interface ServiceInput {
  name: string
  url: string
  interval: number
  mode: WidgetMode
  /** 0 / omitted inherits the global default; 1 disables retries. */
  retryAttempts?: number
  retryDelays?: number[]
}

/** `mode`/`chart` are applied server-side only when set, so a plain drag-save
 *  (which omits them) leaves those preferences alone. */
export interface LayoutItem {
  id: string
  x: number
  y: number
  w: number
  h: number
  mode?: WidgetMode
  chart?: ChartType
}

/** Typed client for the upmonitor REST API. */
export const api = {
  // Setup & auth.
  setupStatus: () => request<{ needsSetup: boolean }>('GET', '/api/setup/status'),
  setup: (username: string, password: string) =>
    request<User>('POST', '/api/setup', { username, password }),
  login: (username: string, password: string) =>
    request<User>('POST', '/api/auth/login', { username, password }),
  logout: () => request<void>('POST', '/api/auth/logout'),
  me: () => request<User>('GET', '/api/auth/me'),

  // Services & metrics.
  listServices: () => request<Service[]>('GET', '/api/services'),
  createService: (input: ServiceInput) => request<Service>('POST', '/api/services', input),
  updateService: (id: string, input: Partial<ServiceInput>) =>
    request<Service>('PUT', `/api/services/${id}`, input),
  deleteService: (id: string) => request<void>('DELETE', `/api/services/${id}`),
  updateLayout: (items: LayoutItem[]) => request<void>('PATCH', '/api/services/layout', items),
  checkNow: (id: string) => request<Service>('POST', `/api/services/${id}/check`),
  serviceMetrics: (id: string, range: MetricsRange = '24h') =>
    request<ServiceMetrics>('GET', `/api/services/${id}/metrics?range=${range}`),
  serviceChecks: (id: string, opts: { limit?: number; before?: number } = {}) => {
    const q = new URLSearchParams()
    if (opts.limit) q.set('limit', String(opts.limit))
    if (opts.before) q.set('before', String(opts.before))
    const qs = q.toString()
    return request<ChecksPage>('GET', `/api/services/${id}/checks${qs ? `?${qs}` : ''}`)
  },
  uploadImage: (id: string, blob: Blob) =>
    requestRaw<{ icon: string }>('POST', `/api/services/${id}/image`, blob, 'image/webp'),
  deleteImage: (id: string) => request<void>('DELETE', `/api/services/${id}/image`),

  // Settings, config & users.
  dashboardStats: () => request<DashboardStats>('GET', '/api/dashboard/stats'),
  getSettings: () => request<AppSettings>('GET', '/api/settings'),
  updateSettings: (settings: AppSettings) => request<AppSettings>('PUT', '/api/settings', settings),
  /** 只提交 agent 段（后端按字段合并；密钥留空＝保持不变）。 */
  updateAgentSettings: (agent: AgentSettings) =>
    request<AppSettings>('PUT', '/api/settings', { agent }),
  setConfigPath: (path: string) =>
    request<AppSettings>('PUT', '/api/settings/config-path', { path }),
  listUsers: () => request<User[]>('GET', '/api/users'),
  createUser: (username: string, password: string, role: UserRole) =>
    request<User>('POST', '/api/users', { username, password, role }),
  deleteUser: (id: number) => request<void>('DELETE', `/api/users/${id}`),

  // Incidents.
  listIncidents: (
    params: {
      status?: string
      severity?: IncidentSeverity
      serviceId?: string
      limit?: number
      offset?: number
    } = {},
  ) => {
    const qs = new URLSearchParams()
    if (params.status) qs.set('status', params.status)
    if (params.severity) qs.set('severity', params.severity)
    if (params.serviceId) qs.set('serviceId', params.serviceId)
    if (params.limit) qs.set('limit', String(params.limit))
    if (params.offset) qs.set('offset', String(params.offset))
    const suffix = qs.toString() ? `?${qs.toString()}` : ''
    return request<IncidentsPage>('GET', `/api/incidents${suffix}`)
  },
  getIncident: (id: number) => request<IncidentDetail>('GET', `/api/incidents/${id}`),
  createIncident: (input: IncidentInput) => request<Incident>('POST', '/api/incidents', input),
  updateIncident: (id: number, input: IncidentInput) =>
    request<Incident>('PUT', `/api/incidents/${id}`, input),
  deleteIncident: (id: number) => request<void>('DELETE', `/api/incidents/${id}`),
  addIncidentComment: (id: number, body: string) =>
    request<IncidentComment>('POST', `/api/incidents/${id}/comments`, { body }),
  listDeliveries: (incidentId: number) =>
    request<Delivery[]>('GET', `/api/incidents/${incidentId}/deliveries`),
  agentReport: (id: number) =>
    request<AgentReport>('GET', `/api/incidents/${id}/agent-report`),
  /** 排队一个排查问题（异步）。回答由 Agent 后台产生，用 agentChatResult 轮询。 */
  agentChatRequest: (
    id: number,
    message: string,
    history: { role: string; text: string }[] = [],
    requestId?: string,
  ) =>
    request<AgentChatRequest>('POST', `/api/incidents/${id}/agent-chat/requests`, {
      message,
      history,
      requestId,
    }),
  agentChatResult: (id: number, requestId: string) =>
    request<AgentChatRequest>('GET', `/api/incidents/${id}/agent-chat/requests/${requestId}`),
  /** 该事件的历史问答，用于刷新页面后恢复会话。 */
  agentChatHistory: (id: number) =>
    request<{ requests: AgentChatRequest[] }>('GET', `/api/incidents/${id}/agent-chat/requests`),
  /** 已废弃的同步路径：服务端仍保留，前端不再使用。 */
  agentChat: (id: number, message: string, history: { role: string; text: string }[] = []) =>
    request<{ reply: string; evidence_ids?: string[] }>('POST', `/api/incidents/${id}/agent-chat`, {
      message,
      history,
    }),
  retryDelivery: (id: number) => request<{ ok: boolean }>('POST', `/api/deliveries/${id}/retry`),

  // Integrations.
  listIntegrations: () => request<Integration[]>('GET', '/api/integrations'),
  createIntegration: (input: IntegrationInput) =>
    request<Integration>('POST', '/api/integrations', input),
  updateIntegration: (id: number, input: IntegrationInput) =>
    request<Integration>('PUT', `/api/integrations/${id}`, input),
  deleteIntegration: (id: number) => request<void>('DELETE', `/api/integrations/${id}`),
  testIntegration: (id: number) =>
    request<{ ok: boolean; error?: string }>('POST', `/api/integrations/${id}/test`),

  // Backup / restore.
  exportConfig: () => getBlob('/api/config/export'),
  importConfig: (archive: Blob) =>
    requestRaw<{ ok: boolean; services: number }>(
      'POST',
      '/api/config/import',
      archive,
      'application/zip',
    ),
}
