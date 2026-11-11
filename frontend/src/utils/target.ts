import type { ResourceKind, TargetConfig, TargetType } from '../api/types'

// 表单选项与校验规则：与后端 store.go 的字段约束保持一致。

export const TARGET_TYPE_OPTIONS: Array<{ label: string; value: TargetType }> = [
  { label: 'HTTP', value: 'HTTP' },
  { label: 'TCP', value: 'TCP' },
  { label: 'K8s 资源', value: 'KUBERNETES' }
]

export const RESOURCE_KIND_OPTIONS: Array<{ label: string; value: ResourceKind }> = [
  { label: 'Deployment', value: 'Deployment' },
  { label: 'StatefulSet', value: 'StatefulSet' }
]

export const TARGET_DEFAULTS = {
  interval_ms: 10000,
  timeout_ms: 3000,
  failure_threshold: 3,
  recovery_threshold: 2
} as const

export const TARGET_KEY_PATTERN = /^[A-Za-z0-9._-]{1,100}$/

/** DNS-1123 subdomain：小写字母/数字/'-'，可点分多段，每段 1–63。 */
export const DNS1123_SUBDOMAIN_PATTERN =
  /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/

export const HTTP_ADDRESS_PATTERN = /^https?:\/\/\S+$/i
export const TCP_ADDRESS_PATTERN = /^(\[[^\]]+\]|[^\s:]+):\d{1,5}$/

export const DNS1123_RULE_TEXT =
  '仅小写字母、数字和 “-”，可用 “.” 分段；每段 1–63 字符，整体不超过 253 字符，且不能以 “-” 开头或结尾'

/**
 * 校验 K8s 资源名（DNS-1123 subdomain）。
 * 返回错误文案；通过时返回 null。
 */
export function validateK8sResourceName(value: string): string | null {
  const name = value.trim()
  if (!name) return '请填写 K8s 资源名'
  if (name.length > 253) return '资源名整体不能超过 253 个字符'
  const segments = name.split('.')
  for (const segment of segments) {
    if (!segment) return `资源名格式不正确：${DNS1123_RULE_TEXT}`
    if (segment.length > 63) return '资源名每段不能超过 63 个字符'
  }
  if (!DNS1123_SUBDOMAIN_PATTERN.test(name)) {
    return `资源名格式不正确：${DNS1123_RULE_TEXT}`
  }
  return null
}

/** 构造提交后端的 config：K8s 只带资源类型/资源名，HTTP/TCP 为空对象。 */
export function buildTargetConfig(
  targetType: TargetType,
  resourceKind: string,
  resourceName: string
): TargetConfig {
  if (targetType !== 'KUBERNETES') return {}
  return {
    resource_kind: resourceKind as ResourceKind,
    resource_name: resourceName.trim()
  }
}

/** 后端对 K8s 目标要求 address 为空字符串。 */
export function buildTargetAddress(targetType: TargetType, address: string): string {
  return targetType === 'KUBERNETES' ? '' : address.trim()
}

/** 毫秒 → 秒数输入（表单用秒展示采样间隔/超时）。 */
export function msToSeconds(value: number): number {
  return Math.round((value / 1000) * 10) / 10
}

export function secondsToMs(value: number): number {
  return Math.round(value * 1000)
}
