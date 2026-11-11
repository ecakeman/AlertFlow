import type { MonitorStatus, Target, TargetConfig, TargetType } from '../api/types'

type TagType = 'success' | 'warning' | 'danger' | 'info' | 'primary'

const STATUS_LABEL: Record<string, string> = {
  NORMAL: '正常',
  ABNORMAL: '异常',
  UNKNOWN: '未知',
  ACTIVE: '活动',
  RESOLVED: '已恢复',
  ARCHIVED: '已归档',
  PENDING: '等待派发',
  DISPATCHED: '已派发',
  RUNNING: '执行中',
  RETRY_WAIT: '重试等待',
  SUCCEEDED: '成功',
  FAILED: '失败',
  DISABLED: '已禁用',
  HEALTHY: '健康',
  UNHEALTHY: '不健康'
}

/** 后端枚举 -> 中文展示；未知枚举原样返回。 */
export function statusLabel(status?: string | null): string {
  if (!status) return '—'
  return STATUS_LABEL[status] ?? status
}

export function statusTagType(status?: string | null): TagType {
  switch (status) {
    case 'NORMAL':
    case 'RESOLVED':
    case 'SUCCEEDED':
    case 'HEALTHY':
      return 'success'
    case 'ABNORMAL':
    case 'FAILED':
    case 'UNHEALTHY':
      return 'danger'
    case 'ACTIVE':
    case 'RUNNING':
    case 'DISPATCHED':
      return 'primary'
    case 'UNKNOWN':
    case 'PENDING':
    case 'RETRY_WAIT':
      return 'warning'
    case 'ARCHIVED':
    case 'DISABLED':
      return 'info'
    default:
      return 'info'
  }
}

export function targetTypeLabel(type?: TargetType | string | null): string {
  if (!type) return '—'
  if (type === 'KUBERNETES') return 'K8S'
  return type
}

export function targetTypeTagType(type?: TargetType | string | null): TagType {
  if (type === 'HTTP') return 'primary'
  if (type === 'TCP') return 'success'
  if (type === 'KUBERNETES') return 'warning'
  return 'info'
}

export function configValue(config: TargetConfig | null | undefined, key: string): string {
  const value = config?.[key]
  return typeof value === 'string' ? value : ''
}

/** 列表里的“摘要”：K8s 显示 资源类型/资源名，HTTP/TCP 显示地址。 */
export function targetSummary(target: Target): string {
  if (target.target_type === 'KUBERNETES') {
    const kind = configValue(target.config, 'resource_kind')
    const name = configValue(target.config, 'resource_name')
    if (kind && name) return `${kind} / ${name}`
    return kind || name || '（配置缺失）'
  }
  return target.address || '—'
}

export function isMonitorStatus(value: string | undefined | null): value is MonitorStatus {
  return value === 'NORMAL' || value === 'ABNORMAL' || value === 'UNKNOWN'
}

export function episodeStatusOptions(): Array<{ label: string; value: string }> {
  return [
    { label: '全部状态', value: '' },
    { label: '活动', value: 'ACTIVE' },
    { label: '已恢复', value: 'RESOLVED' },
    { label: '已归档', value: 'ARCHIVED' }
  ]
}
