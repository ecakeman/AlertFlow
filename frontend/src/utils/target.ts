import type { ResourceKind, TargetConfig, TargetType } from '../api/types'

// 表单选项与校验规则：与后端 store.go 的字段约束保持一致。

export const TARGET_TYPE_OPTIONS: Array<{ label: string; value: TargetType }> = [
  { label: 'HTTP', value: 'HTTP' },
  { label: 'TCP', value: 'TCP' },
  { label: 'KUBERNETES', value: 'KUBERNETES' }
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
export const K8S_RESOURCE_NAME_PATTERN = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
export const HTTP_ADDRESS_PATTERN = /^https?:\/\/\S+$/i
export const TCP_ADDRESS_PATTERN = /^(\[[^\]]+\]|[^\s:]+):\d{1,5}$/

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
