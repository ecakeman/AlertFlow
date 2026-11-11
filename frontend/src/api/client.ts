import axios, { AxiosError } from 'axios'
import type { ApiErrorBody } from './types'

// 所有请求统一走 /api/v1，开发环境由 Vite 代理到 Go Backend。
export const http = axios.create({
  baseURL: '/api/v1',
  timeout: 20000,
  headers: { Accept: 'application/json' }
})

export class ApiError extends Error {
  readonly code: string
  readonly status: number

  constructor(message: string, code: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

http.interceptors.response.use(
  (response) => response,
  (error: AxiosError<ApiErrorBody>) => {
    const status = error.response?.status ?? 0
    const body = error.response?.data
    const message =
      body?.message ||
      (status === 0
        ? '无法连接后端服务，请确认 Go Backend 已启动且 /api 代理可达'
        : error.message || `请求失败（HTTP ${status}）`)
    return Promise.reject(new ApiError(message, body?.error_code || `HTTP_${status}`, status))
  }
)

/** 把任意异常转成可直接展示的中文提示。 */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error) return error.message
  return '未知错误'
}

/** 产物不存在：兼容 404 与部分实现返回的 500/NotFound 文本。 */
export function isArtifactMissing(error: unknown): boolean {
  if (error instanceof ApiError) {
    if (error.status === 404) return true
    return /not\s*found|NOT_FOUND|artifact\s*not/i.test(`${error.code} ${error.message}`)
  }
  return false
}

/** 通用删除/请求包装：成功返回，失败抛出带中文信息的 ApiError。 */
export async function requestVoid(promise: Promise<unknown>): Promise<void> {
  await promise
}
