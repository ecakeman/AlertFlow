<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { fetchEpisodes } from '../api/episodes'
import { fetchServices } from '../api/services'
import { errorMessage } from '../api/client'
import type { EpisodeStatus, EpisodeSummary, ServiceSummary } from '../api/types'
import ErrorRetry from '../components/ErrorRetry.vue'
import StatusTag from '../components/StatusTag.vue'
import { formatDateTime, localTimeZoneLabel, toRfc3339 } from '../utils/time'

const route = useRoute()
const router = useRouter()

const timezone = localTimeZoneLabel()

const episodes = ref<EpisodeSummary[]>([])
const total = ref(0)
const loading = ref(false)
const error = ref('')
const statusFilter = ref<'' | EpisodeStatus>('')
const serviceFilter = ref('')
const dateRange = ref<[Date, Date] | null>(null)
const limit = ref(20)
const offset = ref(0)
const serviceOptions = ref<ServiceSummary[]>([])

const page = computed(() => Math.floor(offset.value / limit.value) + 1)

const statusTabs: Array<{ label: string; value: '' | EpisodeStatus }> = [
  { label: '全部', value: '' },
  { label: '活动 ACTIVE', value: 'ACTIVE' },
  { label: '已恢复 RESOLVED', value: 'RESOLVED' },
  { label: '已归档 ARCHIVED', value: 'ARCHIVED' }
]

async function loadServices(): Promise<void> {
  try {
    const data = await fetchServices({ limit: 200, offset: 0 })
    serviceOptions.value = data.services ?? []
  } catch {
    // 服务筛选器加载失败不阻塞 Episode 列表主流程
    serviceOptions.value = []
  }
}

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const from = dateRange.value ? toRfc3339(dateRange.value[0]) : undefined
    const to = dateRange.value ? toRfc3339(new Date(dateRange.value[1].getTime() + 999)) : undefined
    const data = await fetchEpisodes({
      status: statusFilter.value,
      service_id: serviceFilter.value || undefined,
      from,
      to,
      limit: limit.value,
      offset: offset.value
    })
    episodes.value = data.episodes ?? []
    total.value = data.total ?? 0
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function reloadFirstPage(): void {
  offset.value = 0
  load()
}

function onPageChange(value: number): void {
  offset.value = (value - 1) * limit.value
  load()
}

function onSizeChange(value: number): void {
  limit.value = value
  offset.value = 0
  load()
}

function resetFilters(): void {
  statusFilter.value = ''
  serviceFilter.value = ''
  dateRange.value = null
  reloadFirstPage()
}

function openEpisode(row: EpisodeSummary): void {
  router.push({ name: 'episode-detail', params: { episodeId: row.episode_id } })
}

function openService(row: EpisodeSummary): void {
  router.push({ name: 'service-detail', params: { serviceId: row.service_id } })
}

onMounted(async () => {
  await loadServices()
  const queryService = String(route.query.service_id ?? '')
  if (queryService) serviceFilter.value = queryService
  await load()
})

watch(
  () => route.query.service_id,
  (value) => {
    const next = String(value ?? '')
    if (next && next !== serviceFilter.value) {
      serviceFilter.value = next
      reloadFirstPage()
    }
  }
)
</script>

<template>
  <div>
    <div class="page-toolbar">
      <el-radio-group v-model="statusFilter" @change="reloadFirstPage">
        <el-radio-button v-for="tab in statusTabs" :key="tab.value || 'ALL'" :value="tab.value">
          {{ tab.label }}
        </el-radio-button>
      </el-radio-group>

      <el-select
        v-model="serviceFilter"
        filterable
        clearable
        placeholder="全部服务"
        style="width: 220px"
        @change="reloadFirstPage"
      >
        <el-option
          v-for="item in serviceOptions"
          :key="item.service_id"
          :label="`${item.name}（${item.service_key}）`"
          :value="item.service_id"
        />
      </el-select>

      <el-date-picker
        v-model="dateRange"
        type="datetimerange"
        range-separator="至"
        start-placeholder="开始时间（含）"
        end-placeholder="结束时间（含）"
        :clearable="true"
        style="width: 360px"
        @change="reloadFirstPage"
      />

      <el-button :loading="loading" @click="load">刷新</el-button>
      <el-button @click="resetFilters">重置筛选</el-button>
    </div>

    <ErrorRetry v-if="error" :message="error" :loading="loading" class="section-gap" @retry="load" />

    <el-card shadow="never">
      <template #header>
        <div class="card-header">
          <span class="card-header-title">
            故障 Episode
            <span class="card-header-sub">按开始时间倒序，起止时间按 {{ timezone }} 本地时间筛选</span>
          </span>
          <span class="card-header-sub">共 {{ total }} 条</span>
        </div>
      </template>

      <el-table
        v-loading="loading"
        :data="episodes"
        size="small"
        row-key="episode_id"
        row-class-name="clickable-row"
        @row-click="openEpisode"
      >
        <el-table-column label="Episode" width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.episode_id.slice(0, 8) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="服务" min-width="170" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="clickable" @click.stop="openService(row)">{{ row.service_name || row.service_id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="150">
          <template #default="{ row }">
            <div class="status-cell">
              <StatusTag :status="row.status" />
              <el-tag v-if="row.resolution_pending" size="small" type="warning" effect="plain">恢复待处理</el-tag>
            </div>
          </template>
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
        <el-table-column label="任务状态" width="110">
          <template #default="{ row }">
            <StatusTag v-if="row.active_task_status" :status="row.active_task_status" />
            <span v-else class="text-muted">—</span>
          </template>
        </el-table-column>
        <el-table-column prop="started_by" label="触发者" width="110" show-overflow-tooltip />
        <el-table-column label="更新时间" width="170">
          <template #default="{ row }">{{ formatDateTime(row.updated_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="80" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click.stop="openEpisode(row)">详情</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="没有符合条件的 Episode" :image-size="72" class="empty-compact" />
        </template>
      </el-table>

      <div class="pagination-bar">
        <el-pagination
          :current-page="page"
          :page-size="limit"
          :total="total"
          :page-sizes="[10, 20, 50, 100]"
          layout="total, sizes, prev, pager, next, jumper"
          background
          @current-change="onPageChange"
          @size-change="onSizeChange"
        />
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.status-cell {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-wrap: wrap;
}

.pagination-bar {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}

:deep(.clickable-row) {
  cursor: pointer;
}

:deep(.clickable-row:hover td) {
  background: #f5f7fa;
}
</style>
