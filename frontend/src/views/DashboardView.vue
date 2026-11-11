<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import * as echarts from 'echarts'
import type { ECharts, EChartsOption } from 'echarts'
import { fetchDashboardSummary } from '../api/dashboard'
import { errorMessage } from '../api/client'
import type { DashboardSummary, EpisodeSummary } from '../api/types'
import ErrorRetry from '../components/ErrorRetry.vue'
import StatusTag from '../components/StatusTag.vue'
import { formatClock, formatDateTime, localTimeZoneLabel } from '../utils/time'

const router = useRouter()

const summary = ref<DashboardSummary | null>(null)
const loading = ref(false)
const error = ref('')
const chartEl = ref<HTMLDivElement>()
let chart: ECharts | null = null

const timezone = localTimeZoneLabel()

const trend = computed(() => summary.value?.monitor_trend ?? [])
const hasTrend = computed(() => trend.value.length > 0)
const recentEpisodes = computed<EpisodeSummary[]>(() => summary.value?.recent_episodes ?? [])

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    summary.value = await fetchDashboardSummary()
    await nextTick()
    renderChart()
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function renderChart(): void {
  if (!hasTrend.value) {
    chart?.dispose()
    chart = null
    return
  }
  if (!chartEl.value) return
  if (!chart) chart = echarts.init(chartEl.value)
  const option: EChartsOption = {
    grid: { left: 44, right: 16, top: 34, bottom: 30 },
    tooltip: { trigger: 'axis' },
    legend: { data: ['成功', '失败'], right: 0, top: 0, itemWidth: 12, itemHeight: 12 },
    xAxis: {
      type: 'category',
      boundaryGap: true,
      data: trend.value.map((point) => formatClock(point.bucket_start)),
      axisLabel: { color: '#909399' },
      axisLine: { lineStyle: { color: '#dcdfe6' } }
    },
    yAxis: {
      type: 'value',
      minInterval: 1,
      splitLine: { lineStyle: { color: '#f0f2f5' } },
      axisLabel: { color: '#909399' }
    },
    series: [
      {
        name: '成功',
        type: 'bar',
        stack: 'total',
        barMaxWidth: 28,
        data: trend.value.map((point) => point.success),
        itemStyle: { color: '#67c23a' }
      },
      {
        name: '失败',
        type: 'bar',
        stack: 'total',
        barMaxWidth: 28,
        data: trend.value.map((point) => point.failure),
        itemStyle: { color: '#f56c6c' }
      }
    ]
  }
  chart.setOption(option, true)
  chart.resize()
}

function resizeChart(): void {
  chart?.resize()
}

onMounted(() => {
  load()
  window.addEventListener('resize', resizeChart)
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', resizeChart)
  chart?.dispose()
  chart = null
})

function openEpisode(row: EpisodeSummary): void {
  router.push({ name: 'episode-detail', params: { episodeId: row.episode_id } })
}
</script>

<template>
  <div>
    <div class="page-toolbar">
      <div class="text-muted">
        数据生成时间：{{ formatDateTime(summary?.generated_at) }}
        <span class="tz-inline">（{{ timezone }}）</span>
      </div>
      <div class="toolbar-right">
        <el-tag v-if="summary" size="small" type="info" effect="plain">
          最近 30 分钟采样趋势（5 分钟分桶）
        </el-tag>
        <el-button :loading="loading" @click="load">刷新</el-button>
      </div>
    </div>

    <ErrorRetry v-if="error" :message="error" :loading="loading" class="section-gap" @retry="load" />

    <div v-loading="loading">
      <template v-if="summary">
        <div class="stat-grid">
          <div class="stat-card">
            <span class="stat-label">服务总数</span>
            <span class="stat-value">{{ summary.services.total }}</span>
            <span class="stat-foot">
              正常 {{ summary.services.normal }} · 异常 {{ summary.services.abnormal }} ·
              未知 {{ summary.services.unknown }} · 禁用 {{ summary.services.disabled }}
            </span>
          </div>
          <div class="stat-card">
            <span class="stat-label">服务正常</span>
            <span class="stat-value is-success">{{ summary.services.normal }}</span>
            <span class="stat-foot">监控聚合状态 NORMAL</span>
          </div>
          <div class="stat-card">
            <span class="stat-label">服务异常</span>
            <span class="stat-value is-danger">{{ summary.services.abnormal }}</span>
            <span class="stat-foot">监控聚合状态 ABNORMAL</span>
          </div>
          <div class="stat-card">
            <span class="stat-label">活动 Episode</span>
            <span class="stat-value is-primary">{{ summary.episodes.active }}</span>
            <span class="stat-foot">
              已恢复 {{ summary.episodes.resolved }} · 已归档 {{ summary.episodes.archived }}
            </span>
          </div>
        </div>

        <el-row :gutter="12" class="section-gap">
          <el-col :span="24">
            <el-card shadow="never">
              <template #header>
                <div class="card-header">
                  <span class="card-header-title">健康概览</span>
                  <span class="card-header-sub">实时快照，不含软删除对象</span>
                </div>
              </template>
              <div class="health-grid">
                <div>
                  <div class="health-block-title">服务（按聚合监控状态）</div>
                  <div class="health-row"><span>总数</span><span class="value">{{ summary.services.total }}</span></div>
                  <div class="health-row"><span>正常</span><span class="value">{{ summary.services.normal }}</span></div>
                  <div class="health-row"><span>异常</span><span class="value">{{ summary.services.abnormal }}</span></div>
                  <div class="health-row"><span>未知</span><span class="value">{{ summary.services.unknown }}</span></div>
                  <div class="health-row"><span>已禁用</span><span class="value">{{ summary.services.disabled }}</span></div>
                </div>
                <div>
                  <div class="health-block-title">监控目标</div>
                  <div class="health-row"><span>总数</span><span class="value">{{ summary.targets.total }}</span></div>
                  <div class="health-row"><span>正常</span><span class="value">{{ summary.targets.normal }}</span></div>
                  <div class="health-row"><span>异常</span><span class="value">{{ summary.targets.abnormal }}</span></div>
                  <div class="health-row"><span>未知</span><span class="value">{{ summary.targets.unknown }}</span></div>
                </div>
                <div>
                  <div class="health-block-title">故障 Episode</div>
                  <div class="health-row"><span>活动</span><span class="value">{{ summary.episodes.active }}</span></div>
                  <div class="health-row"><span>已恢复</span><span class="value">{{ summary.episodes.resolved }}</span></div>
                  <div class="health-row"><span>已归档</span><span class="value">{{ summary.episodes.archived }}</span></div>
                </div>
              </div>
            </el-card>
          </el-col>
        </el-row>

        <el-row :gutter="12" class="section-gap">
          <el-col :span="24">
            <el-card shadow="never">
              <template #header>
                <div class="card-header">
                  <span class="card-header-title">
                    监控采样趋势
                    <span class="card-header-sub">GET /dashboard/summary · monitor_trend</span>
                  </span>
                  <span class="card-header-sub">时间轴标签为{{ timezone }}本地时间</span>
                </div>
              </template>
              <div v-if="hasTrend" ref="chartEl" class="chart-box" />
              <el-empty
                v-else
                description="最近 30 分钟窗口内暂无监控采样数据"
                :image-size="72"
                class="empty-compact"
              />
            </el-card>
          </el-col>
        </el-row>

        <el-card shadow="never">
          <template #header>
            <div class="card-header">
              <span class="card-header-title">最近故障</span>
              <span class="card-header-sub">按开始时间倒序，最多 10 条</span>
            </div>
          </template>
          <el-table
            v-if="recentEpisodes.length > 0"
            :data="recentEpisodes"
            size="small"
            row-class-name="clickable-row"
            @row-click="openEpisode"
          >
            <el-table-column label="Episode" width="120">
              <template #default="{ row }">
                <span class="mono">{{ row.episode_id.slice(0, 8) }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="service_name" label="服务" min-width="150" show-overflow-tooltip>
              <template #default="{ row }">
                <span>{{ row.service_name || row.service_id }}</span>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="96">
              <template #default="{ row }">
                <StatusTag :status="row.status" />
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
            <el-table-column label="更新时间" width="170">
              <template #default="{ row }">{{ formatDateTime(row.updated_at) }}</template>
            </el-table-column>
          </el-table>
          <el-empty v-else description="暂无故障记录" :image-size="72" class="empty-compact" />
        </el-card>
      </template>
    </div>

    <span v-if="!summary && !loading && !error" class="text-muted">
      正在加载总览数据…
    </span>
  </div>
</template>

<style scoped>
.tz-inline {
  color: var(--af-text-muted);
}

:deep(.clickable-row) {
  cursor: pointer;
}

:deep(.clickable-row:hover td) {
  background: #f5f7fa;
}
</style>
