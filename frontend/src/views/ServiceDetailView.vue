<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteService, fetchService } from '../api/services'
import { fetchEpisodes } from '../api/episodes'
import { errorMessage } from '../api/client'
import type { EpisodeSummary, ServiceView, Target } from '../api/types'
import ErrorRetry from '../components/ErrorRetry.vue'
import ServiceFormDialog from '../components/ServiceFormDialog.vue'
import StatusTag from '../components/StatusTag.vue'
import TargetFormDialog from '../components/TargetFormDialog.vue'
import { useSessionStore } from '../stores/session'
import { formatDateTime } from '../utils/time'
import { statusLabel, targetSummary, targetTypeLabel, targetTypeTagType } from '../utils/status'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const serviceId = computed(() => String(route.params.serviceId ?? ''))
const service = ref<ServiceView | null>(null)
const activeEpisodes = ref<EpisodeSummary[]>([])
const loading = ref(false)
const error = ref('')
const deleting = ref(false)

const serviceDialogVisible = ref(false)
const targetDialogVisible = ref(false)
const editingTarget = ref<Target | null>(null)

const targets = computed<Target[]>(() => service.value?.targets ?? [])
const aggregationLabel = computed(() => {
  const mode = service.value?.monitor_aggregation_mode
  if (mode === 'ANY_REQUIRED') return 'ANY_REQUIRED（任一必需目标异常即异常）'
  return mode || '—'
})

async function load(): Promise<void> {
  if (!serviceId.value) return
  loading.value = true
  error.value = ''
  try {
    const [view, episodePage] = await Promise.all([
      fetchService(serviceId.value),
      fetchEpisodes({ service_id: serviceId.value, status: 'ACTIVE', limit: 20, offset: 0 })
    ])
    service.value = view
    activeEpisodes.value = episodePage.episodes ?? []
    session.visitService(view.service_id, view.name)
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function onServiceSaved(): void {
  serviceDialogVisible.value = false
  load()
}

function openTargetEdit(row: Target): void {
  editingTarget.value = row
  targetDialogVisible.value = true
}

function onTargetSaved(): void {
  targetDialogVisible.value = false
  load()
}

function openEpisode(row: EpisodeSummary): void {
  router.push({ name: 'episode-detail', params: { episodeId: row.episode_id } })
}

function openEpisodeById(episodeId?: string | null): void {
  if (!episodeId) return
  router.push({ name: 'episode-detail', params: { episodeId } })
}

function openTargetsPage(): void {
  router.push({ name: 'targets', query: { service_id: serviceId.value } })
}

async function removeService(): Promise<void> {
  const current = service.value
  if (!current) return
  try {
    await ElMessageBox.confirm(
      `确认删除服务「${current.name}」？删除后服务与监控目标将被软删除并停止采样，历史故障 Episode、采样点与诊断产物会保留。`,
      '删除服务',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }
    )
  } catch {
    return
  }
  deleting.value = true
  try {
    await deleteService(current.service_id)
    ElMessage.success('服务已删除（历史故障与诊断产物保留）')
    router.push({ name: 'services' })
  } catch (err) {
    ElMessage.error(errorMessage(err))
  } finally {
    deleting.value = false
  }
}

onMounted(load)
watch(serviceId, load)
</script>

<template>
  <div>
    <div class="page-toolbar">
      <el-button link type="primary" @click="router.push({ name: 'services' })">← 返回服务列表</el-button>
      <div class="toolbar-right">
        <el-button :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" plain :disabled="!service" @click="serviceDialogVisible = true">编辑服务</el-button>
        <el-button type="danger" plain :loading="deleting" :disabled="!service" @click="removeService">删除服务</el-button>
      </div>
    </div>

    <ErrorRetry v-if="error" :message="error" :loading="loading" class="section-gap" @retry="load" />

    <div v-loading="loading">
      <template v-if="service">
        <el-card shadow="never" class="section-gap">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">
                {{ service.name }}
                <span class="mono text-muted">{{ service.service_key }}</span>
              </span>
              <div class="header-tags">
                <StatusTag :status="service.monitor_status" />
                <el-tag v-if="!service.enabled" size="small" type="info" effect="plain">已禁用</el-tag>
                <el-tag v-if="service.active_episode_id" size="small" type="danger" effect="plain" class="clickable-tag" @click="openEpisodeById(service.active_episode_id)">
                  活动故障 {{ service.active_episode_id.slice(0, 8) }}
                </el-tag>
              </div>
            </div>
          </template>

          <el-descriptions :column="2" border size="small">
            <el-descriptions-item label="服务 ID">
              <span class="mono">{{ service.service_id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="服务标识">
              <span class="mono">{{ service.service_key }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="监控状态">
              <StatusTag :status="service.monitor_status" />
              <span class="text-muted desc-inline">{{ statusLabel(service.monitor_status) }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="应用状态">
              <StatusTag :status="service.app_status" />
            </el-descriptions-item>
            <el-descriptions-item label="聚合模式">{{ aggregationLabel }}</el-descriptions-item>
            <el-descriptions-item label="权威应用">
              <span class="mono">{{ service.authority_producer_id || '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="启用状态">
              {{ service.enabled ? '启用' : '禁用' }}
            </el-descriptions-item>
            <el-descriptions-item label="描述">{{ service.description || '—' }}</el-descriptions-item>
          </el-descriptions>
        </el-card>

        <el-card shadow="never" class="section-gap">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">
                监控目标
                <span class="card-header-sub">共 {{ targets.length }} 个</span>
              </span>
              <div>
                <el-button size="small" :disabled="!service" @click="openTargetsPage">管理目标</el-button>
              </div>
            </div>
          </template>

          <el-table :data="targets" size="small" row-key="target_id">
            <el-table-column label="目标标识" min-width="140">
              <template #default="{ row }">
                <span class="mono">{{ row.target_key }}</span>
                <div class="text-muted">{{ row.target_name }}</div>
              </template>
            </el-table-column>
            <el-table-column label="类型" width="110">
              <template #default="{ row }">
                <el-tag size="small" :type="targetTypeTagType(row.target_type)" effect="plain">
                  {{ targetTypeLabel(row.target_type) }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="摘要" min-width="220" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="mono">{{ targetSummary(row) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="90">
              <template #default="{ row }"><StatusTag :status="row.status" /></template>
            </el-table-column>
            <el-table-column prop="consecutive_failures" label="连续失败" width="90" align="right" />
            <el-table-column prop="consecutive_successes" label="连续成功" width="90" align="right" />
            <el-table-column label="最近观测时间" width="170">
              <template #default="{ row }">{{ formatDateTime(row.last_check_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="90" fixed="right">
              <template #default="{ row }">
                <el-button link type="primary" size="small" @click="openTargetEdit(row)">编辑</el-button>
              </template>
            </el-table-column>
            <template #empty>
              <el-empty description="暂无监控目标" :image-size="64" class="empty-compact">
                <el-button size="small" type="primary" @click="openTargetsPage">前往新增</el-button>
              </el-empty>
            </template>
          </el-table>
        </el-card>

        <el-card shadow="never">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">
                关联活动 Episode
                <span class="card-header-sub">仅显示 ACTIVE 状态，最多 20 条</span>
              </span>
              <el-button size="small" @click="router.push({ name: 'episodes', query: { service_id: service.service_id } })">
                查看全部
              </el-button>
            </div>
          </template>

          <el-table
            v-if="activeEpisodes.length > 0"
            :data="activeEpisodes"
            size="small"
            row-class-name="clickable-row"
            @row-click="openEpisode"
          >
            <el-table-column label="Episode" width="120">
              <template #default="{ row }"><span class="mono">{{ row.episode_id.slice(0, 8) }}</span></template>
            </el-table-column>
            <el-table-column label="状态" width="90">
              <template #default="{ row }"><StatusTag :status="row.status" /></template>
            </el-table-column>
            <el-table-column label="开始时间" width="170">
              <template #default="{ row }">{{ formatDateTime(row.started_at) }}</template>
            </el-table-column>
            <el-table-column label="恢复时间" width="170">
              <template #default="{ row }">{{ formatDateTime(row.resolved_at) }}</template>
            </el-table-column>
            <el-table-column label="输入版本" width="150">
              <template #default="{ row }">
                <span class="mono">{{ row.input_version }} / {{ row.processed_input_version }}</span>
              </template>
            </el-table-column>
            <el-table-column label="更新时间" width="170">
              <template #default="{ row }">{{ formatDateTime(row.updated_at) }}</template>
            </el-table-column>
          </el-table>
          <el-empty v-else description="当前没有活动故障" :image-size="64" class="empty-compact" />
        </el-card>
      </template>
    </div>

    <ServiceFormDialog
      v-model="serviceDialogVisible"
      mode="edit"
      :service="service"
      @saved="onServiceSaved"
    />

    <TargetFormDialog
      v-model="targetDialogVisible"
      :service-id="serviceId"
      :service-name="service?.name"
      :target="editingTarget"
      @saved="onTargetSaved"
    />
  </div>
</template>

<style scoped>
.header-tags {
  display: flex;
  align-items: center;
  gap: 6px;
}

.clickable-tag {
  cursor: pointer;
}

.desc-inline {
  margin-left: 8px;
}

:deep(.clickable-row) {
  cursor: pointer;
}

:deep(.clickable-row:hover td) {
  background: #f5f7fa;
}
</style>
