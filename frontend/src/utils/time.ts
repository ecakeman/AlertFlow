// 时间格式化与时长计算。时间统一按浏览器本地时区显示，不在页面上重复声明时区；
// 需要溯源时由调用方在 tooltip 中给出原始时间。

const DATE_TIME_FORMATTER = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false
})

const DATE_FORMATTER = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit'
})

const CLOCK_FORMATTER = new Intl.DateTimeFormat('zh-CN', {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false
})

function parse(value: string | null | undefined): Date | null {
  if (!value) return null
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? null : date
}

/** RFC3339 → YYYY-MM-DD HH:mm:ss；空值返回占位符。 */
export function formatDateTime(value: string | null | undefined, placeholder = '—'): string {
  const date = parse(value)
  return date ? DATE_TIME_FORMATTER.format(date) : placeholder
}

/** RFC3339 → YYYY-MM-DD。 */
export function formatDate(value: string | null | undefined, placeholder = '—'): string {
  const date = parse(value)
  return date ? DATE_FORMATTER.format(date) : placeholder
}

/** RFC3339 → HH:mm:ss。 */
export function formatClock(value: string | null | undefined, placeholder = '—'): string {
  const date = parse(value)
  return date ? CLOCK_FORMATTER.format(date) : placeholder
}

/** 给 tooltip 用的原始时间（含本地时区名），用于精确溯源。 */
export function tooltipDateTime(value: string | null | undefined): string {
  const date = parse(value)
  if (!date) return ''
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
  return `${value}（${zone}）`
}

/** 计算 start → end 的耗时；end 为空时用当前时间。 */
export function durationBetween(start: string | null | undefined, end?: string | null): number | null {
  const from = parse(start)
  if (!from) return null
  const to = end === undefined ? new Date() : parse(end)
  if (!to) return null
  return Math.max(0, to.getTime() - from.getTime())
}

/** 毫秒 → 紧凑中文时长：3 秒 / 2 分 15 秒 / 1 小时 4 分 / 2 天 3 小时。 */
export function formatMilliseconds(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return '—'
  const totalSeconds = Math.floor(ms / 1000)
  if (totalSeconds < 60) return `${totalSeconds} 秒`
  const totalMinutes = Math.floor(totalSeconds / 60)
  if (totalMinutes < 60) {
    const seconds = totalSeconds % 60
    return seconds ? `${totalMinutes} 分 ${seconds} 秒` : `${totalMinutes} 分`
  }
  const totalHours = Math.floor(totalMinutes / 60)
  if (totalHours < 24) {
    const minutes = totalMinutes % 60
    return minutes ? `${totalHours} 小时 ${minutes} 分` : `${totalHours} 小时`
  }
  const days = Math.floor(totalHours / 24)
  const hours = totalHours % 24
  return hours ? `${days} 天 ${hours} 小时` : `${days} 天`
}

/** 计算并格式化两个时间之间的耗时。 */
export function formatDuration(start: string | null | undefined, end?: string | null): string {
  return formatMilliseconds(durationBetween(start, end))
}

/** 最近观测时间的展示：空值不显示“刚刚”，而是明确的“尚无观测”。 */
export function formatLastObservation(value: string | null | undefined): string {
  if (!value) return '尚无观测'
  return formatDateTime(value, '尚无观测')
}

/** Date → RFC3339（用于时间范围筛选参数）。 */
export function toRfc3339(date: Date): string {
  return date.toISOString()
}

/** 字节数 → 可读大小。 */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined) return '—'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}
