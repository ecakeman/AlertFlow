import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

// 会话状态：仅当前环境标识、当前页面标题、最近访问服务。
// 不缓存任何业务数据（服务/Episode/产物一律以 API 实时数据为准）。

export interface RecentService {
  serviceId: string
  name: string
  visitedAt: number
}

const ENV_KEY = 'alertflow.env-label'
const RECENT_KEY = 'alertflow.recent-services'
const MAX_RECENT = 8

function readStorage(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

function writeStorage(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value)
  } catch {
    // localStorage 不可用时静默降级为内存状态
  }
}

function detectEnvLabel(): string {
  const host = window.location.hostname
  if (host === 'localhost' || host === '127.0.0.1' || host === '') return '本地环境'
  const port = window.location.port
  return port ? `环境 ${host}:${port}` : `环境 ${host}`
}

function loadRecentServices(): RecentService[] {
  const raw = readStorage(RECENT_KEY)
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw) as RecentService[]
    if (!Array.isArray(parsed)) return []
    return parsed
      .filter((item) => typeof item?.serviceId === 'string' && item.serviceId.length > 0)
      .slice(0, MAX_RECENT)
  } catch {
    return []
  }
}

export const useSessionStore = defineStore('session', () => {
  const envLabel = ref(readStorage(ENV_KEY) || detectEnvLabel())
  const currentTitle = ref('')
  const recentServices = ref<RecentService[]>(loadRecentServices())

  const hasRecentServices = computed(() => recentServices.value.length > 0)

  function setEnvLabel(label: string): void {
    const trimmed = label.trim()
    envLabel.value = trimmed || detectEnvLabel()
    writeStorage(ENV_KEY, envLabel.value)
  }

  function setCurrentTitle(title: string): void {
    currentTitle.value = title
  }

  function visitService(serviceId: string, name: string): void {
    if (!serviceId) return
    const next = recentServices.value.filter((item) => item.serviceId !== serviceId)
    next.unshift({ serviceId, name: name || serviceId, visitedAt: Date.now() })
    recentServices.value = next.slice(0, MAX_RECENT)
    writeStorage(RECENT_KEY, JSON.stringify(recentServices.value))
  }

  function clearRecentServices(): void {
    recentServices.value = []
    writeStorage(RECENT_KEY, '[]')
  }

  return {
    envLabel,
    currentTitle,
    recentServices,
    hasRecentServices,
    setEnvLabel,
    setCurrentTitle,
    visitService,
    clearRecentServices
  }
})
