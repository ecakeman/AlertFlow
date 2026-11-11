import type { EpisodeStatus, MonitorStatus, Target, TargetType } from '../api/types'

export type StatusTone = 'ok' | 'danger' | 'unknown' | 'accent' | 'neutral'

// 状态文案只表达业务含义；UNKNOWN 统一为“尚未观测”，不写“未知/无数据”。
const STATUS_LABEL: Record<string, string> = {
  NORMAL: '正常',
  ABNORMAL: '异常',
  UNKNOWN: '尚未观测',
  ACTIVE: '进行中',
  RESOLVED: '已恢复',
  ARCHIVED: '已归档',
  PENDING: '等待派发',
  DISPATCHED: '已派发',
  RUNNING: '执行中',
  RETRY_WAIT: '重试等待',
  SUCCEEDED: '已完成',
  FAILED: '失败',
  DISABLED: '已禁用',
  HEALTHY: '健康',
  UNHEALTHY: '不健康'
}

const STATUS_TONE: Record<string, StatusTone> = {
  NORMAL: 'ok',
  RESOLVED: 'ok',
  SUCCEEDED: 'ok',
  HEALTHY: 'ok',
  ABNORMAL: 'danger',
  FAILED: 'danger',
  UNHEALTHY: 'danger',
  ACTIVE: 'accent',
  RUNNING: 'accent',
  DISPATCHED: 'accent',
  UNKNOWN: 'unknown',
  PENDING: 'neutral',
  RETRY_WAIT: 'neutral',
  ARCHIVED: 'neutral',
  DISABLED: 'neutral'
}

export function statusLabel(status?: string | null): string {
  if (!status) return '—'
  return STATUS_LABEL[status] ?? status
}

export function statusTone(status?: string | null): StatusTone {
  if (!status) return 'neutral'
  return STATUS_TONE[status] ?? 'neutral'
}

/** 任务类型文案，用于区分“诊断中”和“归档中”。 */
function taskTypeText(taskType?: string | null): string {
  if (taskType === 'DIAGNOSIS') return '诊断'
  if (taskType === 'ARCHIVE') return '归档'
  return ''
}

/** 诊断/归档任务状态文案（列表“诊断状态”列与详情“调查进行中”使用）。 */
export function taskStatusLabel(status?: string | null, taskType?: string | null): string {
  if (!status) return '—'
  const kind = taskTypeText(taskType)
  switch (status) {
    case 'PENDING':
      return '排队中'
    case 'DISPATCHED':
      return '已派发'
    case 'RUNNING':
      return kind ? `${kind}中` : '运行中'
    case 'RETRY_WAIT':
      return '重试等待'
    case 'FAILED':
      return kind ? `${kind}失败` : '任务失败'
    case 'SUCCEEDED':
      return kind ? `${kind}完成` : '已完成'
    default:
      return statusLabel(status)
  }
}

/** 列表里的诊断进度：只表达任务在做什么，没有任务时不伪造结论。 */
export function diagnosisState(
  status?: string | null,
  taskType?: string | null,
  hasReport = false
): { label: string; tone: StatusTone } {
  switch (status) {
    case 'PENDING':
    case 'DISPATCHED':
    case 'RUNNING':
    case 'RETRY_WAIT':
      return { label: taskStatusLabel(status, taskType), tone: 'accent' }
    case 'FAILED':
      return { label: taskStatusLabel(status, taskType), tone: 'danger' }
    case 'SUCCEEDED':
      return { label: taskStatusLabel(status, taskType), tone: 'ok' }
    default:
      return hasReport ? { label: '报告已生成', tone: 'ok' } : { label: '—', tone: 'neutral' }
  }
}

/** 任务是否仍在进行（用于“调查进行中”判断）。 */
export function isTaskInProgress(status?: string | null): boolean {
  return status === 'PENDING' || status === 'DISPATCHED' || status === 'RUNNING' || status === 'RETRY_WAIT'
}

export function isMonitorStatus(value: string): value is MonitorStatus {
  return value === 'NORMAL' || value === 'ABNORMAL' || value === 'UNKNOWN'
}

export function episodeStatusOptions(): Array<{ label: string; value: EpisodeStatus }> {
  return [
    { label: '进行中', value: 'ACTIVE' },
    { label: '已恢复', value: 'RESOLVED' },
    { label: '已归档', value: 'ARCHIVED' }
  ]
}

export function targetTypeLabel(type?: TargetType | string | null): string {
  if (type === 'KUBERNETES') return 'K8s'
  if (type === 'HTTP' || type === 'TCP') return type
  return '—'
}

export function targetSummary(target: Target): string {
  if (target.target_type === 'KUBERNETES') {
    const kind = configValue(target.config, 'resource_kind')
    const name = configValue(target.config, 'resource_name')
    if (kind || name) return [kind, name].filter(Boolean).join(' / ')
  }
  return target.address || '—'
}

export function configValue(
  config: Record<string, unknown> | null | undefined,
  key: string
): string {
  if (!config) return ''
  const value = config[key]
  if (value === null || value === undefined) return ''
  return String(value)
}

/** 监控采样点的检查结果。 */
export function checkStatusLabel(value?: string | null): string {
  if (value === 'SUCCESS') return '采样成功'
  if (value === 'FAILURE') return '采样失败'
  return value || '—'
}

/** 诊断字段里的响应分类（HTTP/K8s 通用）。 */
export function statusClassLabel(value?: string | null): string {
  switch (value) {
    case '2xx':
      return '正常响应'
    case '4xx':
      return '客户端错误'
    case '5xx':
      return '服务端错误'
    case 'timeout':
      return '超时'
    case 'error':
      return '连接错误'
    case 'unhealthy':
      return '状态不健康'
    case '':
    case null:
    case undefined:
      return '—'
    default:
      return value
  }
}

/** 错误分类（技术名属于业务证据，保留原始值作为兜底）。 */
export function errorClassLabel(value?: string | null): string {
  switch (value) {
    case 'connect':
      return '连接失败'
    case 'RESOURCE_NOT_FOUND':
      return '资源不存在'
    case 'timeout':
      return '超时'
    case '':
    case null:
    case undefined:
      return ''
    default:
      return value
  }
}

/** 监控证据一句话摘要：例如“服务端错误（连接失败）”。 */
export function evidenceSummary(statusClass?: string | null, errorClass?: string | null): string {
  const status = statusClassLabel(statusClass)
  const error = errorClassLabel(errorClass)
  if (status === '—' && !error) return '—'
  if (status === '—') return error
  return error ? `${status}（${error}）` : status
}

export function aggregationLabel(mode?: string | null): string {
  if (!mode) return '—'
  if (mode === 'ANY_REQUIRED') return '任一必需监控源正常即视为正常'
  return mode
}
