<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { fetchEpisode, fetchEpisodeArtifact, fetchEpisodeTimeline } from '../api/episodes'
import { errorMessage, isArtifactMissing } from '../api/client'
import type {
  ArtifactRef,
  ArtifactType,
  DiagnosisReport,
  EpisodeArtifact,
  EpisodeTimeline,
  EpisodeView
} from '../api/types'
import ErrorRetry from '../components/ErrorRetry.vue'
import MarkdownViewer from '../components/MarkdownViewer.vue'
import StatusTag from '../components/StatusTag.vue'
import { formatDateTime, formatDuration, localTimeZoneLabel } from '../utils/time'
import { statusLabel, targetTypeLabel, targetTypeTagType } from '../utils/status'

const route = useRoute()
const router = useRouter()

const timezone = localTimeZoneLabel()
const episodeId = computed(() => String(route.params.episodeId ?? ''))

const episode = ref<EpisodeView | null>(null)
const timeline = ref<EpisodeTimeline | null>(null)
const loading = ref(false)
const error = ref('')

const timelineTab = ref<'monitor' | 'events' | 'artifacts'>('monitor')
const artifactTab = ref<ArtifactType>('INVESTIGATION')

interface ArtifactState {
  loading: boolean
  error: string
  data: EpisodeArtifact | null
  missing: boolean
}

const artifactStates = reactive<Record<ArtifactType, ArtifactState>>({
  INVESTIGATION: { loading: false, error: '', data: null, missing: false },
  DIAGNOSIS_REPORT: { loading: false, error: '', data: null, missing: false }
})

const currentArtifact = computed(() => artifactStates[artifactTab.value])

const projection = computed(() => timeline.value?.region1_projection ?? [])
const events = computed(() => timeline.value?.region2_events ?? [])
const artifactRefs = computed<ArtifactRef[]>(() => timeline.value?.region3_refs ?? [])

const refByType = computed<Record<string, ArtifactRef>>(() => {
  const map: Record<string, ArtifactRef> = {}
  for (const item of artifactRefs.value) map[item.artifact_type] = item
  return map
})

const currentRef = computed<ArtifactRef | null>(() => refByType.value[artifactTab.value] ?? null)

const projectionStats = computed(() => {
  const stats = { total: projection.value.length, normal: 0, abnormal: 0, unknown: 0 }
  for (const item of projection.value) {
    if (item.status === 'NORMAL') stats.normal += 1
    else if (item.status === 'ABNORMAL') stats.abnormal += 1
    else stats.unknown += 1
  }
  return stats
})

const diagnosisReport = computed<DiagnosisReport | null>(() => {
  const json = artifactStates.DIAGNOSIS_REPORT.data?.content_json
  if (!json || typeof json !== 'object') return null
  return json as DiagnosisReport
})

const diagnosisSections = computed(() => {
  const report = diagnosisReport.value
  if (!report) return []
  return [
    { key: 'summary', title: '摘要', text: typeof report.summary === 'string' ? report.summary : '' },
    { key: 'observations', title: '观察', items: report.observations ?? [] },
    { key: 'hypotheses', title: '假设', items: report.hypotheses ?? [] },
    { key: 'unknowns', title: '未知项', items: report.unknowns ?? [] },
    { key: 'next_steps', title: '下一步', items: report.next_steps ?? [] }
  ].filter((section) => (section.text ? true : Boolean(section.items?.length)))
})

async function loadArtifact(type: ArtifactType): Promise<void> {
  const state = artifactStates[type]
  const refExists = Boolean(refByType.value[type])
  if (!refExists) {
    state.missing = true
    state.error = ''
    return
  }
  if (state.loading) return
  state.loading = true
  state.error = ''
  state.missing = false
  try {
    state.data = await fetchEpisodeArtifact(episodeId.value, type)
  } catch (err) {
    if (isArtifactMissing(err)) {
      state.missing = true
    } else {
      state.error = errorMessage(err)
    }
  } finally {
    state.loading = false
  }
}

function resetArtifacts(): void {
  for (const type of ['INVESTIGATION', 'DIAGNOSIS_REPORT'] as ArtifactType[]) {
    const state = artifactStates[type]
    state.loading = false
    state.error = ''
    state.data = null
    state.missing = !refByType.value[type]
  }
}

function onArtifactTabChange(name: string | number): void {
  const type = name as ArtifactType
  artifactTab.value = type
  const state = artifactStates[type]
  if (!state.data && !state.missing && !state.loading) {
    void loadArtifact(type)
  }
}

async function load(): Promise<void> {
  if (!episodeId.value) return
  loading.value = true
  error.value = ''
  try {
    const [view, timelineData] = await Promise.all([
      fetchEpisode(episodeId.value),
      fetchEpisodeTimeline(episodeId.value)
    ])
    episode.value = view
    timeline.value = timelineData
    resetArtifacts()
    void loadArtifact(artifactTab.value)
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function openService(): void {
  if (!episode.value) return
  router.push({ name: 'service-detail', params: { serviceId: episode.value.service_id } })
}

function pretty(value: unknown): string {
  if (value === undefined) return 'undefined'
  try {
    return JSON.stringify(value, null, 2) ?? 'null'
  } catch {
    return String(value)
  }
}

function shortHash(hash?: string | null): string {
  if (!hash) return '—'
  return hash.length > 16 ? `${hash.slice(0, 16)}…` : hash
}

function artifactLabel(type: ArtifactType): string {
  return type === 'INVESTIGATION' ? 'investigation.md（调查记录）' : 'diagnosis_report（诊断报告）'
}

function downloadArtifact(): void {
  const data = currentArtifact.value.data
  const type = artifactTab.value
  if (!data) return
  let content: string
  let extension: string
  let mime: string
  if (data.content_text) {
    content = data.content_text
    mime = data.content_type || (type === 'INVESTIGATION' ? 'text/markdown' : 'application/json')
    extension = mime.includes('json') ? 'json' : 'md'
  } else {
    content = JSON.stringify(data.content_json ?? {}, null, 2)
    mime = 'application/json'
    extension = 'json'
  }
  try {
    const blob = new Blob([content], { type: `${mime};charset=utf-8` })
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = `episode-${episodeId.value}-${type.toLowerCase()}-v${data.version}.${extension}`
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
    URL.revokeObjectURL(url)
    ElMessage.success('已生成原文下载')
  } catch (err) {
    ElMessage.error(`下载失败：${errorMessage(err)}`)
  }
}

onMounted(load)
watch(episodeId, load)
</script>

<template>
  <div>
    <div class="page-toolbar">
      <el-button link type="primary" @click="router.push({ name: 'episodes' })">← 返回 Episode 列表</el-button>
      <div class="toolbar-right">
        <el-button :loading="loading" @click="load">刷新</el-button>
      </div>
    </div>

    <ErrorRetry v-if="error" :message="error" :loading="loading" class="section-gap" @retry="load" />

    <div v-loading="loading">
      <template v-if="episode">
        <el-card shadow="never" class="section-gap">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">
                Episode <span class="mono">{{ episode.episode_id }}</span>
              </span>
              <div class="header-tags">
                <StatusTag :status="episode.status" />
                <el-tag v-if="episode.resolution_pending" size="small" type="warning" effect="plain">
                  恢复待处理
                </el-tag>
              </div>
            </div>
          </template>

          <el-descriptions :column="3" border size="small">
            <el-descriptions-item label="服务">
              <span class="clickable" @click="openService">{{ episode.service_name || episode.service_id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="状态">{{ statusLabel(episode.status) }}</el-descriptions-item>
            <el-descriptions-item label="触发者">{{ episode.started_by || '—' }}</el-descriptions-item>
            <el-descriptions-item label="开始时间">{{ formatDateTime(episode.started_at) }}</el-descriptions-item>
            <el-descriptions-item label="恢复时间">{{ formatDateTime(episode.resolved_at) }}</el-descriptions-item>
            <el-descriptions-item label="归档时间">{{ formatDateTime(episode.archived_at) }}</el-descriptions-item>
            <el-descriptions-item label="持续时间">
              {{ formatDuration(episode.started_at, episode.resolved_at) }}
            </el-descriptions-item>
            <el-descriptions-item label="输入版本">
              <span class="mono">
                当前 {{ episode.input_version }} / 已处理 {{ episode.processed_input_version }} / 最终
                {{ episode.final_input_version }}
              </span>
            </el-descriptions-item>
            <el-descriptions-item label="更新时间">{{ formatDateTime(episode.updated_at) }}</el-descriptions-item>
            <el-descriptions-item label="监控状态"><StatusTag :status="episode.monitor_status" /></el-descriptions-item>
            <el-descriptions-item label="应用状态"><StatusTag :status="episode.app_status" /></el-descriptions-item>
            <el-descriptions-item label="活动任务">
              <template v-if="episode.active_task_id">
                <span class="mono">{{ episode.active_task_id.slice(0, 8) }}</span>
                <StatusTag :status="episode.active_task_status" class="task-tag" />
                <span class="text-muted">{{ episode.active_task_type }}</span>
              </template>
              <span v-else class="text-muted">无</span>
            </el-descriptions-item>
            <el-descriptions-item label="输入哈希" :span="3">
              <span class="mono">{{ episode.input_hash || '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="时间显示" :span="3">
              <span class="text-muted">所有时间均由 RFC3339 转换为浏览器本地时区：{{ timezone }}</span>
            </el-descriptions-item>
          </el-descriptions>
        </el-card>

        <el-card shadow="never" class="section-gap">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">
                诊断输入 / 证据
                <span class="card-header-sub">来自 GET /episodes/:id/timeline 的监控投影</span>
              </span>
              <span class="card-header-sub">
                监控目标 {{ projectionStats.total }} · 正常 {{ projectionStats.normal }} · 异常
                {{ projectionStats.abnormal }} · 未知 {{ projectionStats.unknown }}
              </span>
            </div>
          </template>

          <el-descriptions :column="3" border size="small">
            <el-descriptions-item label="输入版本（当前/已处理/最终）">
              <span class="mono">
                {{ episode.input_version }} / {{ episode.processed_input_version }} / {{ episode.final_input_version }}
              </span>
            </el-descriptions-item>
            <el-descriptions-item label="监控投影起点序号">
              <span class="mono">{{ timeline?.monitor_start_point_seq ?? '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="监控/应用状态">
              <StatusTag :status="episode.monitor_status" />
              <StatusTag :status="episode.app_status" class="task-tag" />
            </el-descriptions-item>
          </el-descriptions>

          <el-collapse class="evidence-raw">
            <el-collapse-item title="原始时间轴 JSON（调试用）" name="raw">
              <pre class="json-pre">{{ pretty(timeline) }}</pre>
            </el-collapse-item>
          </el-collapse>
        </el-card>

        <el-card shadow="never" class="section-gap">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">时间轴（三区域）</span>
              <span class="card-header-sub">
                监控投影 {{ projection.length }} 项 · 应用事件 {{ events.length }} 条 · 产物引用
                {{ artifactRefs.length }} 条
              </span>
            </div>
          </template>

          <el-tabs v-model="timelineTab">
            <el-tab-pane label="监控投影" name="monitor">
              <div class="tab-hint text-muted">
                区域一：本次 Episode 输入版本下的目标状态快照（start_point_seq =
                {{ timeline?.monitor_start_point_seq ?? '—' }}）。
              </div>
              <el-table :data="projection" size="small" max-height="420">
                <el-table-column label="目标标识" min-width="140">
                  <template #default="{ row }"><span class="mono">{{ row.target_key }}</span></template>
                </el-table-column>
                <el-table-column label="类型" width="110">
                  <template #default="{ row }">
                    <el-tag size="small" :type="targetTypeTagType(row.target_type)" effect="plain">
                      {{ targetTypeLabel(row.target_type) }}
                    </el-tag>
                  </template>
                </el-table-column>
                <el-table-column label="状态" width="90">
                  <template #default="{ row }"><StatusTag :status="row.status" /></template>
                </el-table-column>
                <el-table-column prop="status_class" label="状态分类" min-width="140" />
                <el-table-column prop="error_class" label="错误分类" min-width="140" />
                <el-table-column label="状态指纹" min-width="180">
                  <template #default="{ row }"><span class="mono">{{ shortHash(row.state_fingerprint) }}</span></template>
                </el-table-column>
                <template #empty>
                  <el-empty description="暂无监控投影数据" :image-size="64" class="empty-compact" />
                </template>
              </el-table>
            </el-tab-pane>

            <el-tab-pane label="应用事件" name="events">
              <div class="tab-hint text-muted">区域二：服务上报的应用事件（received_at 为后端接收时间）。</div>
              <el-table :data="events" size="small" max-height="420">
                <el-table-column type="expand">
                  <template #default="{ row }">
                    <pre class="json-pre">{{ pretty(row.payload) }}</pre>
                  </template>
                </el-table-column>
                <el-table-column label="事件 ID" width="130">
                  <template #default="{ row }"><span class="mono">{{ row.event_id.slice(0, 8) }}</span></template>
                </el-table-column>
                <el-table-column prop="event_kind" label="事件类型" width="140" />
                <el-table-column label="健康状态" width="100">
                  <template #default="{ row }"><StatusTag :status="row.health_status" /></template>
                </el-table-column>
                <el-table-column label="事件时间" width="170">
                  <template #default="{ row }">{{ formatDateTime(row.event_time) }}</template>
                </el-table-column>
                <el-table-column label="接收时间" width="170">
                  <template #default="{ row }">{{ formatDateTime(row.received_at) }}</template>
                </el-table-column>
                <el-table-column label="记录 ID" min-width="140">
                  <template #default="{ row }"><span class="mono">{{ row.event_record_id.slice(0, 8) }}</span></template>
                </el-table-column>
                <template #empty>
                  <el-empty description="暂无应用事件" :image-size="64" class="empty-compact" />
                </template>
              </el-table>
            </el-tab-pane>

            <el-tab-pane label="产物引用" name="artifacts">
              <div class="tab-hint text-muted">区域三：磁盘上的诊断产物引用（不内联内容，点击下方产物 Tab 读取原文）。</div>
              <el-table :data="artifactRefs" size="small" max-height="420">
                <el-table-column label="产物类型" width="180">
                  <template #default="{ row }"><span class="mono">{{ row.artifact_type }}</span></template>
                </el-table-column>
                <el-table-column prop="artifact_version" label="版本" width="80" align="right" />
                <el-table-column prop="content_type" label="内容类型" width="150" />
                <el-table-column label="存储键" min-width="220" show-overflow-tooltip>
                  <template #default="{ row }"><span class="mono">{{ row.storage_key }}</span></template>
                </el-table-column>
                <el-table-column label="内容哈希" width="190">
                  <template #default="{ row }"><span class="mono">{{ shortHash(row.content_hash) }}</span></template>
                </el-table-column>
                <el-table-column label="更新时间" width="170">
                  <template #default="{ row }">{{ formatDateTime(row.updated_at) }}</template>
                </el-table-column>
                <template #empty>
                  <el-empty description="尚未生成任何产物" :image-size="64" class="empty-compact" />
                </template>
              </el-table>
            </el-tab-pane>
          </el-tabs>
        </el-card>

        <el-card shadow="never">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">诊断产物</span>
              <span class="card-header-sub">
                内容来自 GET /api/v1/episodes/:id/artifacts?type=…，Markdown 经 marked + DOMPurify 安全渲染
              </span>
            </div>
          </template>

          <el-tabs v-model="artifactTab" @tab-change="onArtifactTabChange">
            <el-tab-pane label="investigation.md" name="INVESTIGATION">
              <template v-if="!refByType.INVESTIGATION && !artifactStates.INVESTIGATION.data">
                <el-empty description="尚未生成" :image-size="64" class="empty-compact">
                  <template #description>
                    <div>尚未生成调查记录（investigation.md）</div>
                    <div class="text-muted empty-sub">该 Episode 当前版本还没有 INVESTIGATION 产物。</div>
                  </template>
                </el-empty>
              </template>
            </el-tab-pane>
            <el-tab-pane label="diagnosis_report" name="DIAGNOSIS_REPORT">
              <template v-if="!refByType.DIAGNOSIS_REPORT && !artifactStates.DIAGNOSIS_REPORT.data">
                <el-empty description="尚未生成" :image-size="64" class="empty-compact">
                  <template #description>
                    <div>尚未生成诊断报告（diagnosis_report）</div>
                    <div class="text-muted empty-sub">该 Episode 当前版本还没有 DIAGNOSIS_REPORT 产物。</div>
                  </template>
                </el-empty>
              </template>
            </el-tab-pane>
          </el-tabs>

          <!-- 当前产物内容（两个 Tab 共用渲染区，数据按 artifactTab 取） -->
          <template v-if="currentArtifact.data || currentArtifact.loading || currentArtifact.error">
            <el-divider />
            <div class="artifact-toolbar">
              <span class="mono">{{ artifactLabel(artifactTab) }}</span>
              <el-tag v-if="currentArtifact.data" size="small" type="info" effect="plain">
                版本 {{ currentArtifact.data.version }}
              </el-tag>
              <el-tag v-if="currentArtifact.data" size="small" type="info" effect="plain">
                {{ currentArtifact.data.content_type || '未知内容类型' }}
              </el-tag>
              <span v-if="currentArtifact.data" class="text-muted mono">
                哈希 {{ shortHash(currentArtifact.data.content_hash) }}
              </span>
              <span v-if="currentRef" class="text-muted">
                产物更新时间 {{ formatDateTime(currentRef.updated_at) }}
              </span>
              <div class="toolbar-right">
                <el-button size="small" :loading="currentArtifact.loading" @click="loadArtifact(artifactTab)">
                  重新获取
                </el-button>
                <el-button size="small" type="primary" plain :disabled="!currentArtifact.data" @click="downloadArtifact">
                  下载原文
                </el-button>
              </div>
            </div>

            <div v-if="currentArtifact.loading" class="artifact-loading">
              <el-skeleton :rows="6" animated />
            </div>

            <ErrorRetry
              v-else-if="currentArtifact.error"
              :message="currentArtifact.error"
              :loading="currentArtifact.loading"
              @retry="loadArtifact(artifactTab)"
            />

            <template v-else-if="currentArtifact.data">
              <template v-if="artifactTab === 'INVESTIGATION'">
                <MarkdownViewer v-if="currentArtifact.data.content_text" :content="currentArtifact.data.content_text" />
                <el-empty v-else description="产物内容为空" :image-size="64" class="empty-compact" />
              </template>

              <template v-else>
                <MarkdownViewer v-if="currentArtifact.data.content_text" :content="currentArtifact.data.content_text" />
                <template v-else-if="diagnosisSections.length > 0">
                  <div v-for="section in diagnosisSections" :key="section.key" class="report-section">
                    <div class="report-section-title">{{ section.title }}</div>
                    <div v-if="section.text" class="report-text">{{ section.text }}</div>
                    <ul v-else class="report-list">
                      <li v-for="(item, index) in section.items" :key="index">{{ item }}</li>
                    </ul>
                  </div>
                  <el-collapse class="evidence-raw">
                    <el-collapse-item title="原始 JSON" name="json">
                      <pre class="json-pre">{{ pretty(currentArtifact.data.content_json) }}</pre>
                    </el-collapse-item>
                  </el-collapse>
                </template>
                <template v-else>
                  <el-empty description="诊断报告暂无结构化内容" :image-size="64" class="empty-compact">
                    <el-button size="small" @click="downloadArtifact">下载原始 JSON</el-button>
                  </el-empty>
                </template>
              </template>
            </template>
          </template>
        </el-card>
      </template>
    </div>
  </div>
</template>

<style scoped>
.header-tags {
  display: flex;
  align-items: center;
  gap: 6px;
}

.task-tag {
  margin-left: 6px;
}

.tab-hint {
  font-size: 12px;
  margin-bottom: 8px;
}

.evidence-raw {
  margin-top: 10px;
}

.empty-sub {
  font-size: 12px;
  margin-top: 4px;
}

.artifact-loading {
  padding: 8px 0;
}

.report-text {
  line-height: 1.7;
  white-space: pre-wrap;
}
</style>
