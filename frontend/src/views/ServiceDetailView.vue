<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  deleteService,
  deleteTarget,
  fetchService,
  fetchServiceTimeline,
  patchTarget
} from '../api/services'
import { fetchEpisodes } from '../api/episodes'
import { errorMessage } from '../api/client'
import type { EpisodeSummary, ServiceTimelineRecord, ServiceView, Target } from '../api/types'
import PageHeader from '../components/PageHeader.vue'
import StateBlock from '../components/StateBlock.vue'
import StatusTag from '../components/StatusTag.vue'
import DualStatus from '../components/DualStatus.vue'
import TargetFormDrawer from '../components/TargetFormDrawer.vue'
import ServiceFormDialog from '../components/ServiceFormDialog.vue'
import {
  aggregationLabel,
  checkStatusLabel,
  diagnosisState,
  evidenceSummary,
  targetTypeLabel
} from '../utils/status'
import {
  formatDateTime,
  formatDuration,
  formatLastObservation,
  tooltipDateTime
} from '../utils/time'

const route = useRoute()
const router = useRouter()

const serviceId = computed(() => String(route.params.serviceId ?? ''))

const service = ref<ServiceView | null>(null)
const episodes = ref<EpisodeSummary[]>([])
const loading = ref(false)
const error = ref('')
const activeTab = ref<'overview' | 'monitors' | 'events'>('overview')

const timeline = ref<ServiceTimelineRecord[]>([])
const timelineLoading = ref(false)
const timelineError = ref('')
const timelineLoaded = ref(false)

const targetDrawerVisible = ref(false)
const editingTarget = ref<Target | null>(null)
const serviceDialogVisible = ref(false)
const deletingTargetId = ref('')

const activeEpisodes = computed(() => episodes.value.filter((episode) => episode.status === 'ACTIVE'))

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [view, page] = await Promise.all([
      fetchService(serviceId.value),
      fetchEpisodes({ service_id: serviceId.value, limit: 20 })
    ])
    service.value = view
    episodes.value = page.episodes
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

async function loadTimeline() {
  if (timelineLoaded.value) return
  timelineLoading.value = true
  timelineError.value = ''
  try {
    const data = await fetchServiceTimeline(serviceId.value, 100)
    timeline.value = data.records
    timelineLoaded.value = true
  } catch (err) {
    timelineError.value = errorMessage(err)
  } finally {
    timelineLoading.value = false
  }
}

watch(activeTab, (tab) => {
  if (tab === 'events') loadTimeline()
})

function openCreateTarget() {
  editingTarget.value = null
  targetDrawerVisible.value = true
}

function openEditTarget(target: Target) {
  editingTarget.value = target
  targetDrawerVisible.value = true
}

async function toggleTarget(target: Target, enabled: boolean) {
  try {
    await patchTarget(target.target_id, { enabled })
    ElMessage.success(enabled ? '监控源已启用' : '监控源已停用')
    load()
  } catch (err) {
    ElMessage.error(errorMessage(err))
  }
}

async function removeTarget(target: Target) {
  try {
    await ElMessageBox.confirm(
      '删除后该监控源不再采样；历史采样点与故障 Episode 会保留。',
      `删除监控源「${target.target_name}」`,
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }
  deletingTargetId.value = target.target_id
  try {
    await deleteTarget(target.target_id)
    ElMessage.success('监控源已删除')
    load()
  } catch (err) {
    ElMessage.error(errorMessage(err))
  } finally {
    deletingTargetId.value = ''
  }
}

async function removeService() {
  if (!service.value) return
  try {
    await ElMessageBox.confirm(
      '删除后，该服务不再出现在目录中；历史故障 Episode、采样点与诊断产物都会保留，便于回溯。',
      `删除服务「${service.value.name}」`,
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }
  try {
    await deleteService(service.value.service_id)
    ElMessage.success('服务已删除')
    router.push('/services')
  } catch (err) {
    ElMessage.error(errorMessage(err))
  }
}

function serviceDeleted(serviceIdValue: string | number | object) {
  if (serviceIdValue === 'delete') removeService()
}

function episodeDuration(episode: EpisodeSummary): string {
  if (episode.resolved_at) return `持续 ${formatDuration(episode.started_at, episode.resolved_at)}`
  return `进行中 · ${formatDuration(episode.started_at)}`
}

onMounted(load)
</script>

<template>
  <div>
    <PageHeader
      :title="service?.name ?? '服务详情'"
      :subtitle="service ? `服务标识 ${service.service_key}` : undefined"
      back-to="/services"
      back-text="返回服务列表"
    >
      <template #title-extra>
        <StatusTag v-if="service && !service.enabled" status="DISABLED" />
      </template>
      <template #meta>
        <div v-if="service" class="page-meta">
          <DualStatus :monitor-status="service.monitor_status" :app-status="service.app_status" inline />
          <span :title="tooltipDateTime(service.last_check_at)">
            最近观测 {{ formatLastObservation(service.last_check_at) }}
          </span>
          <span class="mono">{{ service.targets.length }} 个监控源</span>
        </div>
      </template>
      <template #actions>
        <el-button :loading="loading" @click="load">
          <el-icon><Refresh /></el-icon>
          <span>刷新</span>
        </el-button>
        <el-button :disabled="!service" @click="serviceDialogVisible = true">编辑</el-button>
        <el-dropdown v-if="service" trigger="click" @command="serviceDeleted">
          <el-button>
            更多<el-icon><ArrowDown /></el-icon>
          </el-button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item command="delete">删除服务</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </template>
    </PageHeader>

    <StateBlock v-if="loading && !service" mode="loading" />
    <StateBlock v-else-if="error && !service" mode="error" :message="error" @retry="load" />

    <template v-else-if="service">
      <el-tabs v-model="activeTab">
        <el-tab-pane label="概览" name="overview">
          <div class="overview-grid">
            <section class="af-panel">
              <div class="af-panel-head">
                <div class="af-panel-title">当前健康</div>
              </div>
              <div class="kv-list">
                <div class="kv-row">
                  <div class="kv-key">监控状态</div>
                  <div class="kv-value"><StatusTag :status="service.monitor_status" /></div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">应用状态</div>
                  <div class="kv-value">
                    <StatusTag :status="service.app_status" />
                    <div class="form-hint">应用状态由应用侧事件上报决定，与监控采样相互独立。</div>
                  </div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">最近观测</div>
                  <div class="kv-value num" :title="tooltipDateTime(service.last_check_at)">
                    {{ formatLastObservation(service.last_check_at) }}
                  </div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">聚合方式</div>
                  <div class="kv-value">{{ aggregationLabel(service.monitor_aggregation_mode) }}</div>
                </div>
                <div class="kv-row">
                  <div class="kv-key">应用生产者</div>
                  <div class="kv-value mono">{{ service.authority_producer_id || '—' }}</div>
                </div>
              </div>
            </section>

            <section class="af-panel">
              <div class="af-panel-head">
                <div class="af-panel-title">
                  活动故障
                  <span class="af-panel-sub">当前尚未恢复的 Episode</span>
                </div>
              </div>
              <StateBlock v-if="activeEpisodes.length === 0" mode="empty" message="当前没有进行中的故障" compact />
              <div v-else class="recent-list">
                <div v-for="episode in activeEpisodes" :key="episode.episode_id" class="recent-row">
                  <div class="recent-main">
                    <div class="recent-name">
                      <router-link :to="`/episodes/${episode.episode_id}`">查看故障详情</router-link>
                      <StatusTag :status="episode.status" size="small" />
                    </div>
                    <div class="recent-meta">
                      <span>开始于 {{ formatDateTime(episode.started_at) }}</span>
                      <span>{{ diagnosisState(episode.active_task_status, episode.active_task_type).label }}</span>
                    </div>
                  </div>
                </div>
              </div>
            </section>

            <section class="af-panel overview-grid-span">
              <div class="af-panel-head">
                <div class="af-panel-title">服务说明</div>
              </div>
              <div class="af-panel-body">
                <p v-if="service.description" class="description-text">{{ service.description }}</p>
                <span v-else class="text-muted">未填写服务说明。</span>
              </div>
            </section>
          </div>
        </el-tab-pane>

        <el-tab-pane label="监控源" name="monitors">
          <section class="af-panel">
            <div class="af-panel-head">
              <div class="af-panel-title">
                监控源
                <span class="af-panel-sub">HTTP / TCP / K8s 资源的观测配置</span>
              </div>
              <div class="af-panel-actions">
                <el-button size="small" type="primary" @click="openCreateTarget">
                  <el-icon><Plus /></el-icon>
                  <span>新增监控源</span>
                </el-button>
              </div>
            </div>
            <el-table :data="service.targets" row-key="target_id" style="width: 100%">
              <el-table-column label="名称" min-width="200">
                <template #default="{ row }">
                  <div class="service-cell">
                    <span class="service-name">{{ row.target_name }}</span>
                    <div class="service-key mono">{{ row.target_key }}</div>
                  </div>
                </template>
              </el-table-column>
              <el-table-column label="类型" width="110">
                <template #default="{ row }">
                  <span>{{ targetTypeLabel(row.target_type) }}</span>
                </template>
              </el-table-column>
              <el-table-column label="状态" width="140">
                <template #default="{ row }">
                  <StatusTag v-if="row.enabled" :status="row.status" />
                  <StatusTag v-else status="DISABLED" />
                </template>
              </el-table-column>
              <el-table-column label="最近观测" min-width="180">
                <template #default="{ row }">
                  <span
                    class="text-secondary num"
                    :title="tooltipDateTime(row.last_check_at)"
                  >{{ formatLastObservation(row.last_check_at) }}</span>
                </template>
              </el-table-column>
              <el-table-column label="操作" width="210" fixed="right">
                <template #default="{ row }">
                  <div class="row-actions">
                    <span class="table-link" @click="openEditTarget(row)">编辑</span>
                    <el-switch
                      :model-value="row.enabled"
                      size="small"
                      :loading="false"
                      @change="(value: string | number | boolean) => toggleTarget(row, Boolean(value))"
                    />
                    <span
                      class="table-link is-danger"
                      :class="{ 'is-loading': deletingTargetId === row.target_id }"
                      @click="removeTarget(row)"
                    >删除</span>
                  </div>
                </template>
              </el-table-column>
              <template #empty>
                <StateBlock mode="empty" message="还没有监控源，先新增一个 HTTP、TCP 或 K8s 监控源">
                  <el-button size="small" type="primary" @click="openCreateTarget">新增监控源</el-button>
                </StateBlock>
              </template>
            </el-table>
          </section>
        </el-tab-pane>

        <el-tab-pane label="事件记录" name="events">
          <section class="af-panel">
            <div class="af-panel-head">
              <div class="af-panel-title">
                监控观测
                <span class="af-panel-sub">服务下所有监控源最近的采样结果</span>
              </div>
            </div>
            <StateBlock v-if="timelineLoading" mode="loading" compact />
            <StateBlock v-else-if="timelineError" mode="error" :message="timelineError" compact @retry="loadTimeline" />
            <StateBlock v-else-if="timeline.length === 0" mode="empty" message="还没有监控采样记录" compact />
            <div v-else class="event-list">
              <div v-for="record in timeline" :key="record.record_id" class="event-item">
                <div class="event-time" :title="tooltipDateTime(record.event_time)">
                  {{ formatDateTime(record.event_time) }}
                </div>
                <div class="event-body">
                  <div class="event-head">
                    <span class="event-source">监控采样</span>
                    <StatusTag
                      :status="record.content?.check_status === 'SUCCESS' ? 'NORMAL' : 'ABNORMAL'"
                      :label="checkStatusLabel(record.content?.check_status)"
                      size="small"
                    />
                    <span class="event-detail">
                      {{ evidenceSummary(record.content?.diagnostic_fields?.status_class, record.content?.diagnostic_fields?.error_class) }}
                    </span>
                  </div>
                </div>
              </div>
            </div>
          </section>

          <section class="af-panel">
            <div class="af-panel-head">
              <div class="af-panel-title">
                故障记录
                <span class="af-panel-sub">该服务的历史 Episode</span>
              </div>
              <router-link :to="`/episodes?service_id=${service.service_id}`" class="text-small">查看全部</router-link>
            </div>
            <StateBlock v-if="episodes.length === 0" mode="empty" message="该服务还没有故障记录" compact />
            <el-table v-else :data="episodes" row-key="episode_id" style="width: 100%">
              <el-table-column label="状态" width="110">
                <template #default="{ row }"><StatusTag :status="row.status" size="small" /></template>
              </el-table-column>
              <el-table-column label="开始时间" min-width="180">
                <template #default="{ row }">
                  <span class="num" :title="tooltipDateTime(row.started_at)">{{ formatDateTime(row.started_at) }}</span>
                </template>
              </el-table-column>
              <el-table-column label="持续 / 恢复" min-width="200">
                <template #default="{ row }">
                  <span class="num">{{ episodeDuration(row) }}</span>
                  <div v-if="row.resolved_at" class="text-muted text-small num">
                    恢复于 {{ formatDateTime(row.resolved_at) }}
                  </div>
                </template>
              </el-table-column>
              <el-table-column label="诊断状态" width="150">
                <template #default="{ row }">
                  <StatusTag
                    :label="diagnosisState(row.active_task_status, row.active_task_type).label"
                    :tone="diagnosisState(row.active_task_status, row.active_task_type).tone"
                    size="small"
                  />
                </template>
              </el-table-column>
              <el-table-column label="操作" width="90" fixed="right">
                <template #default="{ row }">
                  <router-link :to="`/episodes/${row.episode_id}`" class="table-link">查看</router-link>
                </template>
              </el-table-column>
            </el-table>
          </section>
        </el-tab-pane>
      </el-tabs>
    </template>

    <TargetFormDrawer
      v-model="targetDrawerVisible"
      :service-id="serviceId"
      :target="editingTarget"
      @saved="load"
    />
    <ServiceFormDialog v-model="serviceDialogVisible" :service="service" @saved="load" />
  </div>
</template>
