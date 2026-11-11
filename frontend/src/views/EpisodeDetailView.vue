<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { fetchEpisode, fetchEpisodeArtifact, fetchEpisodeTimeline } from '../api/episodes'
import { errorMessage, isArtifactMissing } from '../api/client'
import type {
  ArtifactType,
  DiagnosisReport,
  EpisodeArtifact,
  EpisodeTimeline,
  EpisodeView
} from '../api/types'
import PageHeader from '../components/PageHeader.vue'
import StateBlock from '../components/StateBlock.vue'
import StatusTag from '../components/StatusTag.vue'
import DualStatus from '../components/DualStatus.vue'
import MarkdownViewer from '../components/MarkdownViewer.vue'
import {
  checkStatusLabel,
  evidenceSummary,
  isTaskInProgress,
  statusLabel,
  targetTypeLabel,
  taskStatusLabel
} from '../utils/status'
import type { StatusTone } from '../utils/status'
import {
  formatClock,
  formatDateTime,
  formatDuration,
  tooltipDateTime
} from '../utils/time'

const route = useRoute()
const episodeId = computed(() => String(route.params.episodeId ?? ''))

const episode = ref<EpisodeView | null>(null)
const timeline = ref<EpisodeTimeline | null>(null)
const loading = ref(false)
const error = ref('')
const activeTab = ref<'events' | 'investigation'>('events')

type ArtifactLoadState = 'idle' | 'loading' | 'ready' | 'missing' | 'error'

interface ArtifactState {
  state: ArtifactLoadState
  data: EpisodeArtifact | null
  error: string
}

const artifactStates = reactive<Record<ArtifactType, ArtifactState>>({
  INVESTIGATION: { state: 'idle', data: null, error: '' },
  DIAGNOSIS_REPORT: { state: 'idle', data: null, error: '' }
})

const ARTIFACT_LABEL: Record<ArtifactType, string> = {
  INVESTIGATION: '调查记录',
  DIAGNOSIS_REPORT: '诊断报告'
}

const report = computed<DiagnosisReport | null>(() => {
  const json = artifactStates.DIAGNOSIS_REPORT.data?.content_json
  return (json as DiagnosisReport | null | undefined) ?? null
})

const reportSummary = computed(() => (report.value?.summary ?? '').trim())
const reportSections = computed(() => {
  const data = report.value
  if (!data) return []
  const sections: Array<{ title: string; items: string[] }> = []
  if (data.observations?.length) sections.push({ title: '关键观察', items: data.observations })
  if (data.hypotheses?.length) sections.push({ title: '可能原因', items: data.hypotheses })
  if (data.unknowns?.length) sections.push({ title: '待确认信息', items: data.unknowns })
  if (data.next_steps?.length) sections.push({ title: '后续动作', items: data.next_steps })
  return sections
})

const investigationInProgress = computed(
  () => episode.value?.status === 'ACTIVE' && isTaskInProgress(episode.value.active_task_status)
)

const projection = computed(() => timeline.value?.region1_projection ?? [])
const appEvents = computed(() => timeline.value?.region2_events ?? [])
const artifactRefs = computed(() => timeline.value?.region3_refs ?? [])
const monitorPoints = computed(() => timeline.value?.monitor_points ?? [])

const recentAppEvents = computed(() =>
  [...appEvents.value].sort((a, b) => new Date(b.event_time).getTime() - new Date(a.event_time).getTime()).slice(0, 3)
)

interface TimelineEntry {
  id: string
  time: string
  receivedAt?: string
  source: string
  title: string
  detail?: string
  tone: StatusTone
  statusText?: string
}

const timelineEntries = computed<TimelineEntry[]>(() => {
  const data = timeline.value
  const current = episode.value
  if (!data || !current) return []

  const entries: TimelineEntry[] = []

  entries.push({
    id: 'episode-start',
    time: current.started_at,
    source: '故障开始',
    title: `进入 Episode，监控状态 ${statusLabel(current.monitor_status)}`,
    detail: `应用状态 ${statusLabel(current.app_status)}`,
    tone: 'accent'
  })

  for (const point of monitorPoints.value) {
    entries.push({
      id: `monitor-${point.point_seq}`,
      time: point.observed_at,
      source: `监控 · ${point.target_key || targetTypeLabel(point.target_type)}`,
      title: checkStatusLabel(point.check_status),
      detail: evidenceSummary(point.status_class, point.error_class),
      tone: point.check_status === 'SUCCESS' ? 'ok' : 'danger',
      statusText: point.check_status === 'SUCCESS' ? '成功' : '失败'
    })
  }

  for (const event of appEvents.value) {
    entries.push({
      id: `app-${event.event_record_id}`,
      time: event.event_time,
      receivedAt: event.received_at,
      source: `应用 · ${event.event_kind || '事件上报'}`,
      title: event.health_status ? `应用状态 ${statusLabel(event.health_status)}` : '应用事件上报',
      detail: describePayload(event.payload),
      tone: event.health_status === 'UNHEALTHY' ? 'danger' : 'neutral'
    })
  }

  if (current.resolved_at) {
    entries.push({
      id: 'episode-resolved',
      time: current.resolved_at,
      source: '故障恢复',
      title: `Episode 已恢复，持续 ${formatDuration(current.started_at, current.resolved_at)}`,
      tone: 'ok'
    })
  }
  if (current.archived_at) {
    entries.push({
      id: 'episode-archived',
      time: current.archived_at,
      source: '归档',
      title: '诊断产物已归档',
      tone: 'neutral'
    })
  }

  return entries.sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime())
})

const technicalJson = computed(() => {
  if (!episode.value || !timeline.value) return ''
  return JSON.stringify(
    {
      episode: episode.value,
      monitor_start_point_seq: timeline.value.monitor_start_point_seq,
      monitor_points: timeline.value.monitor_points,
      region1_projection: timeline.value.region1_projection,
      region2_events: timeline.value.region2_events,
      region3_refs: timeline.value.region3_refs
    },
    null,
    2
  )
})

function describePayload(payload: unknown): string {
  if (payload === null || payload === undefined) return ''
  if (typeof payload === 'string') return payload
  if (typeof payload === 'object') {
    const record = payload as Record<string, unknown>
    for (const key of ['message', 'reason', 'detail', 'summary']) {
      const value = record[key]
      if (typeof value === 'string' && value.trim()) return value
    }
    const text = JSON.stringify(record)
    return text.length > 180 ? `${text.slice(0, 180)}…` : text
  }
  return String(payload)
}

function receivedLater(entry: TimelineEntry): string {
  if (!entry.receivedAt) return ''
  const eventTime = new Date(entry.time).getTime()
  const receivedTime = new Date(entry.receivedAt).getTime()
  if (Number.isNaN(eventTime) || Number.isNaN(receivedTime)) return ''
  if (Math.abs(receivedTime - eventTime) < 1000) return ''
  return `接收于 ${formatClock(entry.receivedAt)}`
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [view, time] = await Promise.all([
      fetchEpisode(episodeId.value),
      fetchEpisodeTimeline(episodeId.value)
    ])
    episode.value = view
    timeline.value = time
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
  if (!error.value) {
    await loadArtifact('DIAGNOSIS_REPORT')
  }
}

function artifactRef(type: ArtifactType) {
  return artifactRefs.value.find((ref) => ref.artifact_type === type)
}

async function loadArtifact(type: ArtifactType): Promise<EpisodeArtifact | null> {
  const current = artifactStates[type]
  if (current.state === 'ready' && current.data) return current.data
  if (!artifactRef(type)) {
    current.state = 'missing'
    current.data = null
    return null
  }
  current.state = 'loading'
  current.error = ''
  try {
    const data = await fetchEpisodeArtifact(episodeId.value, type)
    current.data = data
    current.state = 'ready'
    return data
  } catch (err) {
    if (isArtifactMissing(err)) {
      current.state = 'missing'
      current.data = null
      return null
    }
    current.state = 'error'
    current.error = errorMessage(err)
    return null
  }
}

async function retryArtifact(type: ArtifactType) {
  artifactStates[type].state = 'idle'
  await loadArtifact(type)
}

function downloadArtifact(type: ArtifactType) {
  const artifact = artifactStates[type].data
  if (!artifact) return
  const text =
    artifact.content_text?.trim() ||
    JSON.stringify(artifact.content_json ?? {}, null, 2)
  const extension = (artifact.content_type || '').includes('json') ? 'json' : 'md'
  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `episode-${episodeId.value.slice(0, 8)}-${type.toLowerCase()}-v${artifact.version}.${extension}`
  document.body.appendChild(anchor)
  anchor.click()
  document.body.removeChild(anchor)
  URL.revokeObjectURL(url)
}

async function downloadFromRef(type: ArtifactType) {
  await loadArtifact(type)
  if (artifactStates[type].state === 'ready') downloadArtifact(type)
}

watch(activeTab, (tab) => {
  if (tab === 'investigation' && artifactStates.INVESTIGATION.state === 'idle') {
    loadArtifact('INVESTIGATION')
  }
})

onMounted(load)

function rawTime(value: string): string {
  return tooltipDateTime(value)
}

function episodeDurationText(): string {
  if (!episode.value) return ''
  if (episode.value.resolved_at) {
    return `持续 ${formatDuration(episode.value.started_at, episode.value.resolved_at)}`
  }
  return `进行中 · ${formatDuration(episode.value.started_at)}`
}
</script>

<template>
  <div>
    <PageHeader
      :title="episode?.service_name ?? '故障详情'"
      :subtitle="episode ? `故障开始于 ${formatDateTime(episode.started_at)}` : undefined"
      back-to="/episodes"
      back-text="返回故障记录"
    >
      <template #title-extra>
        <StatusTag v-if="episode" :status="episode.status" />
      </template>
      <template #meta>
        <div v-if="episode" class="page-meta">
          <span class="num">{{ episodeDurationText() }}</span>
          <DualStatus :monitor-status="episode.monitor_status" :app-status="episode.app_status" inline />
          <span v-if="episode.resolved_at" :title="rawTime(episode.resolved_at)" class="num">
            恢复于 {{ formatDateTime(episode.resolved_at) }}
          </span>
        </div>
      </template>
      <template #actions>
        <el-button :loading="loading" @click="load">
          <el-icon><Refresh /></el-icon>
          <span>刷新</span>
        </el-button>
      </template>
    </PageHeader>

    <StateBlock v-if="loading && !episode" mode="loading" />
    <StateBlock v-else-if="error && !episode" mode="error" :message="error" @retry="load" />

    <div v-else-if="episode" class="detail-layout">
      <div class="detail-main">
        <section class="af-panel">
          <div class="af-panel-head">
            <div class="af-panel-title">诊断结论</div>
            <div class="af-panel-actions">
              <span v-if="artifactRef('DIAGNOSIS_REPORT')" class="text-small text-secondary">
                报告 v{{ artifactRef('DIAGNOSIS_REPORT')?.artifact_version }}
              </span>
            </div>
          </div>

          <StateBlock
            v-if="artifactStates.DIAGNOSIS_REPORT.state === 'loading'"
            mode="loading"
            compact
          />
          <StateBlock
            v-else-if="artifactStates.DIAGNOSIS_REPORT.state === 'error'"
            mode="error"
            :message="artifactStates.DIAGNOSIS_REPORT.error"
            compact
            @retry="retryArtifact('DIAGNOSIS_REPORT')"
          />
          <template v-else-if="reportSummary">
            <div class="af-panel-body">
              <p class="conclusion-text">{{ reportSummary }}</p>
              <div v-if="reportSections.length" class="conclusion-sections">
                <div v-for="section in reportSections" :key="section.title" class="conclusion-section">
                  <div class="conclusion-section-title">{{ section.title }}</div>
                  <ul>
                    <li v-for="(item, index) in section.items" :key="index">{{ item }}</li>
                  </ul>
                </div>
              </div>
            </div>
          </template>
          <template v-else-if="investigationInProgress">
            <div class="af-panel-body">
              <div class="investigating">
                <el-icon class="is-loading"><Loading /></el-icon>
                <div>
                  <div class="investigating-title">调查进行中</div>
                  <div class="text-secondary text-small">
                    当前诊断任务：{{ taskStatusLabel(episode.active_task_status, episode.active_task_type) }}。Agent 尚未给出最终结论，完成后会在此展示诊断报告。
                  </div>
                </div>
              </div>
            </div>
          </template>
          <StateBlock
            v-else
            mode="empty"
            :message="
              artifactStates.DIAGNOSIS_REPORT.state === 'missing'
                ? '这次故障没有生成诊断报告'
                : '暂无诊断结论'
            "
            compact
          />
        </section>

        <section class="af-panel">
          <div class="af-panel-body" style="padding-bottom: 0">
            <el-tabs v-model="activeTab">
              <el-tab-pane label="事件过程" name="events">
                <div class="event-list">
                  <StateBlock v-if="timelineEntries.length === 0" mode="empty" message="该故障没有可展示的监控或应用事件" />
                  <div v-for="entry in timelineEntries" :key="entry.id" class="event-item">
                    <div class="event-time" :title="rawTime(entry.time)">
                      {{ formatDateTime(entry.time) }}
                    </div>
                    <div class="event-body">
                      <div class="event-head">
                        <span class="event-source"><strong>{{ entry.source }}</strong></span>
                        <StatusTag
                          v-if="entry.statusText"
                          :label="entry.statusText"
                          :tone="entry.tone"
                          size="small"
                        />
                      </div>
                      <div class="event-content">{{ entry.title }}</div>
                      <div v-if="entry.detail" class="event-detail">{{ entry.detail }}</div>
                      <div v-if="receivedLater(entry)" class="event-received">{{ receivedLater(entry) }}</div>
                    </div>
                  </div>
                </div>
              </el-tab-pane>

              <el-tab-pane label="调查记录" name="investigation">
                <StateBlock
                  v-if="artifactStates.INVESTIGATION.state === 'loading'"
                  mode="loading"
                  compact
                />
                <StateBlock
                  v-else-if="artifactStates.INVESTIGATION.state === 'error'"
                  mode="error"
                  :message="artifactStates.INVESTIGATION.error"
                  compact
                  @retry="retryArtifact('INVESTIGATION')"
                />
                <StateBlock
                  v-else-if="artifactStates.INVESTIGATION.state === 'missing'"
                  mode="empty"
                  message="该故障没有调查记录产物"
                  compact
                />
                <div v-else class="af-panel-body">
                  <MarkdownViewer
                    v-if="artifactStates.INVESTIGATION.data?.content_text"
                    :content="artifactStates.INVESTIGATION.data.content_text"
                  />
                  <span v-else class="text-muted">调查记录内容为空。</span>
                </div>
              </el-tab-pane>
            </el-tabs>
          </div>
        </section>

        <section class="af-panel">
          <el-collapse>
            <el-collapse-item name="technical">
              <template #title>
                <span class="af-panel-title">技术详情</span>
                <span class="af-panel-sub" style="margin-left: 8px">输入版本、任务与原始数据</span>
              </template>
              <div class="kv-list">
                <div class="kv-row">
                  <div class="kv-key">输入版本</div>
                  <div class="kv-value num">
                    {{ episode.input_version }}（已处理 {{ episode.processed_input_version }}，最终
                    {{ episode.final_input_version ?? '—' }}）
                  </div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">输入哈希</div>
                  <div class="kv-value mono">{{ episode.input_hash || '—' }}</div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">诊断任务</div>
                  <div class="kv-value">
                    <span class="mono">{{ episode.active_task_id || '—' }}</span>
                    <span v-if="episode.active_task_type" class="text-secondary"> · {{ episode.active_task_type }}</span>
                    <span v-if="episode.active_task_status" class="text-secondary"> · {{ taskStatusLabel(episode.active_task_status, episode.active_task_type) }}</span>
                  </div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">监控起点</div>
                  <div class="kv-value num">point_seq {{ timeline?.monitor_start_point_seq ?? '—' }}</div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">Episode ID</div>
                  <div class="kv-value mono">{{ episode.episode_id }}</div>
                </div>
              </div>
              <div class="af-panel-body">
                <div class="text-small text-secondary" style="margin-bottom: 8px">原始时间线 JSON</div>
                <pre class="json-pre">{{ technicalJson }}</pre>
              </div>
            </el-collapse-item>
          </el-collapse>
        </section>
      </div>

      <aside class="detail-side">
        <section class="af-panel">
          <div class="af-panel-head">
            <div class="af-panel-title">监控证据</div>
          </div>
          <StateBlock
            v-if="projection.length === 0"
            mode="empty"
            message="尚无监控投影快照"
            compact
          />
          <div v-else class="evidence-list">
            <div v-for="field in projection" :key="`${field.target_key}-${field.target_type}`" class="evidence-row">
              <div class="evidence-main">
                <div class="evidence-name">
                  {{ field.target_key || '未命名监控源' }}
                  <span class="text-muted text-small">{{ targetTypeLabel(field.target_type) }}</span>
                </div>
                <div class="evidence-detail">{{ evidenceSummary(field.status_class, field.error_class) }}</div>
              </div>
              <div class="evidence-side"><StatusTag :status="field.status" size="small" /></div>
            </div>
          </div>
        </section>

        <section class="af-panel">
          <div class="af-panel-head">
            <div class="af-panel-title">应用状态与关键事件</div>
          </div>
          <div class="kv-list">
            <div class="kv-row">
              <div class="kv-key">应用状态</div>
              <div class="kv-value"><StatusTag :status="episode.app_status" /></div>
            </div>
          </div>
          <StateBlock v-if="recentAppEvents.length === 0" mode="empty" message="故障期间没有应用事件上报" compact />
          <div v-else class="evidence-list">
            <div v-for="event in recentAppEvents" :key="event.event_record_id" class="evidence-row">
              <div class="evidence-main">
                <div class="evidence-name">{{ event.event_kind || '应用事件' }}</div>
                <div class="evidence-detail" :title="rawTime(event.event_time)">
                  {{ formatDateTime(event.event_time) }}
                  <span v-if="event.health_status"> · {{ statusLabel(event.health_status) }}</span>
                </div>
              </div>
            </div>
          </div>
        </section>

        <section class="af-panel">
          <div class="af-panel-head">
            <div class="af-panel-title">诊断产物</div>
          </div>
          <StateBlock v-if="artifactRefs.length === 0" mode="empty" message="暂无诊断产物" compact />
          <div v-else class="artifact-list">
            <div v-for="ref in artifactRefs" :key="ref.artifact_type" class="artifact-row">
              <div class="artifact-main">
                <div class="artifact-name">
                  {{ ARTIFACT_LABEL[ref.artifact_type as ArtifactType] ?? ref.artifact_type }}
                </div>
                <div class="artifact-meta">
                  v{{ ref.artifact_version }} · {{ formatDateTime(ref.updated_at) }}
                </div>
              </div>
              <span
                class="table-link"
                @click="downloadFromRef(ref.artifact_type as ArtifactType)"
              >下载</span>
            </div>
          </div>
        </section>
      </aside>
    </div>
  </div>
</template>
