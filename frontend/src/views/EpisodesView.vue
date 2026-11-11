<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { fetchEpisodes } from '../api/episodes'
import { fetchServices } from '../api/services'
import { errorMessage } from '../api/client'
import type { EpisodeStatus, EpisodeSummary, ServiceSummary } from '../api/types'
import PageHeader from '../components/PageHeader.vue'
import StateBlock from '../components/StateBlock.vue'
import StatusTag from '../components/StatusTag.vue'
import { diagnosisState, episodeStatusOptions } from '../utils/status'
import { formatDateTime, formatDuration, toRfc3339, tooltipDateTime } from '../utils/time'

const route = useRoute()
const router = useRouter()

const episodes = ref<EpisodeSummary[]>([])
const total = ref(0)
const loading = ref(false)
const error = ref('')

const services = ref<ServiceSummary[]>([])
const status = ref<EpisodeStatus | ''>('')
const serviceId = ref('')
const range = ref<[Date, Date] | null>(null)
const page = ref(1)
const pageSize = ref(20)

const offset = computed(() => (page.value - 1) * pageSize.value)

async function loadEpisodes() {
  loading.value = true
  error.value = ''
  try {
    const data = await fetchEpisodes({
      status: status.value,
      service_id: serviceId.value || undefined,
      from: range.value ? toRfc3339(range.value[0]) : undefined,
      to: range.value ? toRfc3339(range.value[1]) : undefined,
      limit: pageSize.value,
      offset: offset.value
    })
    episodes.value = data.episodes
    total.value = data.total
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

async function loadServices() {
  try {
    const data = await fetchServices({ limit: 200 })
    services.value = data.services
  } catch {
    // 服务筛选只是辅助，加载失败不阻塞故障列表
    services.value = []
  }
}

function syncQuery() {
  const query: Record<string, string> = {}
  if (status.value) query.status = status.value
  if (serviceId.value) query.service_id = serviceId.value
  router.replace({ query }).catch(() => undefined)
}

function applyFilters() {
  page.value = 1
  syncQuery()
  loadEpisodes()
}

function resetFilters() {
  status.value = ''
  serviceId.value = ''
  range.value = null
  applyFilters()
}

function changePage(next: number) {
  page.value = next
  loadEpisodes()
}

function changePageSize(size: number) {
  pageSize.value = size
  page.value = 1
  loadEpisodes()
}

function openEpisode(episode: EpisodeSummary) {
  router.push(`/episodes/${episode.episode_id}`)
}

function durationText(episode: EpisodeSummary): string {
  if (episode.resolved_at) return `持续 ${formatDuration(episode.started_at, episode.resolved_at)}`
  return `进行中 · ${formatDuration(episode.started_at)}`
}

onMounted(() => {
  const queryStatus = String(route.query.status ?? '')
  if (queryStatus === 'ACTIVE' || queryStatus === 'RESOLVED' || queryStatus === 'ARCHIVED') {
    status.value = queryStatus
  }
  serviceId.value = String(route.query.service_id ?? '')
  loadServices()
  loadEpisodes()
})
</script>

<template>
  <div>
    <PageHeader title="故障记录" subtitle="服务故障的时间线、状态与诊断进度">
      <template #actions>
        <el-button :loading="loading" @click="loadEpisodes">
          <el-icon><Refresh /></el-icon>
          <span>刷新</span>
        </el-button>
      </template>
    </PageHeader>

    <div class="toolbar">
      <div class="filter-bar">
        <el-select v-model="status" style="width: 130px" @change="applyFilters">
          <el-option label="全部状态" value="" />
          <el-option
            v-for="option in episodeStatusOptions()"
            :key="option.value"
            :label="option.label"
            :value="option.value"
          />
        </el-select>
        <el-select
          v-model="serviceId"
          style="width: 220px"
          filterable
          clearable
          placeholder="全部服务"
          @change="applyFilters"
        >
          <el-option v-for="service in services" :key="service.service_id" :label="service.name" :value="service.service_id" />
        </el-select>
        <el-date-picker
          v-model="range"
          type="datetimerange"
          range-separator="至"
          start-placeholder="开始时间"
          end-placeholder="结束时间"
          :clearable="true"
          @change="applyFilters"
        />
        <el-button v-if="status || serviceId || range" text @click="resetFilters">清空筛选</el-button>
      </div>
      <div class="text-small text-secondary">共 {{ total }} 条，按开始时间倒序</div>
    </div>

    <div class="af-panel">
      <StateBlock v-if="loading && episodes.length === 0" mode="loading" />
      <StateBlock v-else-if="error && episodes.length === 0" mode="error" :message="error" @retry="loadEpisodes" />
      <el-table
        v-else
        v-loading="loading"
        :data="episodes"
        row-key="episode_id"
        style="width: 100%"
        @row-click="openEpisode"
      >
        <el-table-column label="服务" min-width="200">
          <template #default="{ row }">
            <router-link :to="`/episodes/${row.episode_id}`" class="service-name" @click.stop>
              {{ row.service_name }}
            </router-link>
          </template>
        </el-table-column>
        <el-table-column label="Episode 状态" min-width="150">
          <template #default="{ row }">
            <div class="episode-status-cell">
              <StatusTag :status="row.status" />
              <span v-if="row.resolution_pending" class="text-muted text-small">待归档</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="开始时间" min-width="180">
          <template #default="{ row }">
            <span class="num" :title="tooltipDateTime(row.started_at)">{{ formatDateTime(row.started_at) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="持续 / 恢复" min-width="210">
          <template #default="{ row }">
            <span class="num">{{ durationText(row) }}</span>
            <div v-if="row.resolved_at" class="text-muted text-small num">
              恢复于 {{ formatDateTime(row.resolved_at) }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="诊断状态" min-width="170">
          <template #default="{ row }">
            <StatusTag
              :label="diagnosisState(row.active_task_status, row.active_task_type, row.has_diagnosis_report).label"
              :tone="diagnosisState(row.active_task_status, row.active_task_type, row.has_diagnosis_report).tone"
            />
          </template>
        </el-table-column>
        <template #empty>
          <StateBlock
            mode="empty"
            :message="status || serviceId || range ? '没有匹配的故障记录，试试调整筛选条件' : '还没有故障记录'"
          />
        </template>
      </el-table>

      <div v-if="total > 0" class="pagination-bar">
        <el-pagination
          :current-page="page"
          :page-size="pageSize"
          :page-sizes="[20, 50, 100]"
          :total="total"
          layout="total, sizes, prev, pager, next"
          background
          @current-change="changePage"
          @size-change="changePageSize"
        />
      </div>
    </div>
  </div>
</template>
