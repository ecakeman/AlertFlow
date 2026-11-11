import { http } from './client'
import type {
  ServiceCreatePayload,
  ServicePatchPayload,
  ServiceTimeline,
  ServiceView,
  ServicesPage,
  Target,
  TargetInput
} from './types'

export interface ServiceListParams {
  limit?: number
  offset?: number
  status?: 'NORMAL' | 'ABNORMAL' | 'UNKNOWN' | ''
  /** 服务端模糊搜索：匹配服务名称与 service_key（不区分大小写）。 */
  q?: string
}

export async function fetchServices(params: ServiceListParams = {}): Promise<ServicesPage> {
  const query: Record<string, string | number> = {
    limit: params.limit ?? 50,
    offset: params.offset ?? 0
  }
  if (params.status) query.status = params.status
  const keyword = params.q?.trim()
  if (keyword) query.q = keyword
  const { data } = await http.get<ServicesPage>('/services', { params: query })
  return data
}

export async function fetchService(serviceId: string): Promise<ServiceView> {
  const { data } = await http.get<ServiceView>(`/services/${encodeURIComponent(serviceId)}`)
  return data
}

export async function createService(payload: ServiceCreatePayload): Promise<ServiceView> {
  const { data } = await http.post<ServiceView>('/services', payload)
  return data
}

export async function patchService(serviceId: string, payload: ServicePatchPayload): Promise<ServiceView> {
  const { data } = await http.patch<ServiceView>(`/services/${encodeURIComponent(serviceId)}`, payload)
  return data
}

export async function deleteService(serviceId: string): Promise<void> {
  await http.delete(`/services/${encodeURIComponent(serviceId)}`)
}

export async function fetchServiceTargets(serviceId: string): Promise<Target[]> {
  const { data } = await http.get<{ targets: Target[] | null }>(
    `/services/${encodeURIComponent(serviceId)}/targets`
  )
  return data.targets ?? []
}

/** 服务级监控观测记录，最多 200 条（后端默认 50，倒序返回）。 */
export async function fetchServiceTimeline(serviceId: string, limit = 100): Promise<ServiceTimeline> {
  const { data } = await http.get<ServiceTimeline>(`/services/${encodeURIComponent(serviceId)}/timeline`, {
    params: { limit }
  })
  return { records: data.records ?? [] }
}

export async function createTarget(serviceId: string, payload: TargetInput): Promise<Target> {
  const { data } = await http.post<Target>(
    `/services/${encodeURIComponent(serviceId)}/targets`,
    payload as unknown as Record<string, unknown>
  )
  return data
}

export type TargetPatchPayload = Partial<Omit<TargetInput, 'target_key' | 'target_type'>>

export async function patchTarget(targetId: string, payload: TargetPatchPayload): Promise<Target> {
  const { data } = await http.patch<Target>(
    `/targets/${encodeURIComponent(targetId)}`,
    payload as unknown as Record<string, unknown>
  )
  return data
}

export async function deleteTarget(targetId: string): Promise<void> {
  await http.delete(`/targets/${encodeURIComponent(targetId)}`)
}
