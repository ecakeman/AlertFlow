// 时间展示统一在这里处理：API 返回 RFC3339（UTC），页面统一转成浏览器本地时区并显式标注。
const pad2 = (n: number): string => String(n).padStart(2, '0')

/** RFC3339 -> 本地时间 "YYYY-MM-DD HH:mm:ss"；空值显示占位符。 */
export function formatDateTime(value?: string | null, placeholder = '—'): string {
  if (!value) return placeholder
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return (
    `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())} ` +
    `${pad2(date.getHours())}:${pad2(date.getMinutes())}:${pad2(date.getSeconds())}`
  )
}

/** 只显示时分秒，用于趋势图坐标轴。 */
export function formatClock(value?: string | null): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return `${pad2(date.getHours())}:${pad2(date.getMinutes())}`
}

/** 持续时间（开始 -> 结束/当前），用于故障时长展示。 */
export function formatDuration(start?: string | null, end?: string | null): string {
  if (!start) return '—'
  const from = new Date(start).getTime()
  const to = end ? new Date(end).getTime() : Date.now()
  if (Number.isNaN(from) || Number.isNaN(to) || to < from) return '—'
  const seconds = Math.floor((to - from) / 1000)
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时 ${minutes % 60} 分`
  const days = Math.floor(hours / 24)
  return `${days} 天 ${hours % 24} 小时`
}

/** 本地时区标注，例如 "Asia/Shanghai (UTC+08:00)"。 */
export function localTimeZoneLabel(): string {
  let zone = '未知时区'
  try {
    zone = Intl.DateTimeFormat().resolvedOptions().timeZone || zone
  } catch {
    // 某些受限环境拿不到时区名，退回偏移量
  }
  const offsetMinutes = -new Date().getTimezoneOffset()
  const sign = offsetMinutes >= 0 ? '+' : '-'
  const abs = Math.abs(offsetMinutes)
  return `${zone} (UTC${sign}${pad2(Math.floor(abs / 60))}:${pad2(abs % 60)})`
}

/** Date -> RFC3339（用于 Episode 筛选的 from/to）。 */
export function toRfc3339(date: Date): string {
  return date.toISOString()
}

/** 字节数转可读大小。 */
export function formatBytes(size: number): string {
  if (!Number.isFinite(size) || size < 0) return '—'
  if (size < 1024) return `${size} B`
  const units = ['KB', 'MB', 'GB']
  let value = size / 1024
  let unitIndex = 0
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024
    unitIndex += 1
  }
  return `${value.toFixed(1)} ${units[unitIndex]}`
}
