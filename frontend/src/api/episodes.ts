import { http } from './client'
import type { ArtifactType, EpisodeArtifact, EpisodeSummary, EpisodeTimeline, EpisodeView, EpisodesPage } from './types'

export interface EpisodeListParams {
  status?: 'ACTIVE' | 'RESOLVED' | 'ARCHIVED' | ''
  service_id?: string
  from?: string
  to?: string
  limit?: number
  offset?: number
}

export async function fetchEpisodes(params: EpisodeListParams = {}): Promise<EpisodesPage> {
  const query: Record<string, string | number> = {
    limit: params.limit ?? 20,
    offset: params.offset ?? 0
  }
  if (params.status) query.status = params.status
  if (params.service_id) query.service_id = params.service_id
  if (params.from) query.from = params.from
  if (params.to) query.to = params.to
  const { data } = await http.get<EpisodesPage>('/episodes', { params: query })
  return data
}

export async function fetchEpisode(episodeId: string): Promise<EpisodeView> {
  const { data } = await http.get<EpisodeView>(`/episodes/${encodeURIComponent(episodeId)}`)
  return data
}

export async function fetchEpisodeTimeline(episodeId: string): Promise<EpisodeTimeline> {
  const { data } = await http.get<EpisodeTimeline>(`/episodes/${encodeURIComponent(episodeId)}/timeline`)
  return data
}

export async function fetchEpisodeArtifact(
  episodeId: string,
  type: ArtifactType
): Promise<EpisodeArtifact> {
  const { data } = await http.get<EpisodeArtifact>(`/episodes/${encodeURIComponent(episodeId)}/artifacts`, {
    params: { type }
  })
  return data
}

export type { EpisodeSummary }
