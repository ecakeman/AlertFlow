<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import * as echarts from 'echarts'
import { fetchDashboardSummary } from '../api/dashboard'
import { fetchServices } from '../api/services'
import { errorMessage } from '../api/client'
import type { DashboardSummary, ServiceSummary } from '../api/types'
import PageHeader from '../components/PageHeader.vue'
import StateBlock from '../components/StateBlock.vue'
import StatusTag from '../components/StatusTag.vue'
import DualStatus from '../components/DualStatus.vue'
import { diagnosisState, statusLabel } from '../utils/status'
import {
  formatClock,
  formatDateTime,
  formatDuration,
  formatLastObservation,
  tooltipDateTime
} from '../utils/time'

const router = useRouter()

const summary = ref<DashboardSummary | null>(null)
const attentionServices = ref<ServiceSummary[]>([])
const loading = ref(false)
const error = ref('')

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null

const trendPoints = computed(() => summary.value?.monitor_trend ?? [])
const hasTrend = computed(() =>
  trendPoints.value.some((point) => point.success + point.failure > 0)
)
const updatedLabel = computed(() =>
  summary.value ? `数据更新于 ${formatDateTime(summary.value.generated_at)}` : ''
)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [dashboard, abnormal] = await Promise.all([
      fetchDashboardSummary(),
      fetchServices({ status: 'ABNORMAL', limit: 20 })
    ])
    summary.value = dashboard
    attentionServices.value = abnormal.services
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
  await nextTick()
  renderChart()
}

function renderChart() {
  if (!chartEl.value || !hasTrend.value) {
    chart?.dispose()
    chart = null
    return
  }
  if (!chart) chart = echarts.init(chartEl.value)
  const points = trendPoints.value
  chart.setOption(
    {
      grid: { left: 4, right: 8, top: 26, bottom: 2, containLabel: true },
      legend: {
        right: 0,
        top: 0,
        itemWidth: 8,
        itemHeight: 8,
        textStyle: { color: '#65727d', fontSize: 11 }
      },
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'shadow' },
        textStyle: { fontSize: 12 }
      },
      xAxis: {
        type: 'category',
        data: points.map((point) => formatClock(point.bucket_start)),
        axisLine: { lineStyle: { color: '#dde3e7' } },
        axisTick: { show: false },
        axisLabel: { color: '#8b959e', fontSize: 11 }
      },
      yAxis: {
        type: 'value',
        minInterval: 1,
        splitLine: { lineStyle: { color: '#eef1f3' } },
        axisLabel: { color: '#8b959e', fontSize: 11 }
      },
      series: [
        {
          name: '成功',
          type: 'bar',
          stack: 'samples',
          barMaxWidth: 14,
          itemStyle: { color: '#2e755c' },
          data: points.map((point) => point.success)
        },
        {
          name: '失败',
          type: 'bar',
          stack: 'samples',
          barMaxWidth: 14,
          itemStyle: { color: '#b83e3a' },
          data: points.map((point) => point.failure)
        }
      ]
    },
    true
  )
}

function handleResize() {
  chart?.resize()
}

function openEpisode(episodeId: string) {
  router.push(`/episodes/${episodeId}`)
}

function recentDuration(startedAt: string, resolvedAt: string | null): string {
  if (resolvedAt) return `持续 ${formatDuration(startedAt, resolvedAt)}`
  return `进行中 · ${formatDuration(startedAt)}`
}

onMounted(() => {
  load()
  window.addEventListener('resize', handleResize)
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', handleResize)
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div>
    <PageHeader title="运行总览" subtitle="服务健康与近期故障">
      <template #actions>
        <span v-if="updatedLabel" class="text-small text-secondary">{{ updatedLabel }}</span>
        <el-button :loading="loading" @click="load">
          <el-icon><Refresh /></el-icon>
          <span>刷新</span>
        </el-button>
      </template>
    </PageHeader>

    <StateBlock v-if="loading && !summary" mode="loading" />
    <StateBlock v-else-if="error && !summary" mode="error" :message="error" @retry="load" />

    <template v-else-if="summary">
      <div class="summary-strip">
        <router-link to="/episodes?status=ACTIVE" class="summary-item">
          <span class="summary-value" :class="{ 'is-danger': summary.episodes.active > 0 }">
            {{ summary.episodes.active }}
          </span>
          <span class="summary-label">活动故障</span>
        </router-link>
        <router-link to="/services?status=ABNORMAL" class="summary-item">
          <span class="summary-value" :class="{ 'is-danger': summary.services.abnormal > 0 }">
            {{ summary.services.abnormal }}
          </span>
          <span class="summary-label">异常服务</span>
        </router-link>
        <router-link to="/services" class="summary-item">
          <span class="summary-value">{{ summary.targets.total }}</span>
          <span class="summary-label">监控目标</span>
          <span class="summary-hint">正常 {{ summary.targets.normal }}</span>
        </router-link>
      </div>

      <div class="overview-grid">
        <section class="af-panel">
          <div class="af-panel-head">
            <div class="af-panel-title">
              需要关注
              <span class="af-panel-sub">监控或应用状态异常的服务</span>
            </div>
            <router-link to="/services?status=ABNORMAL" class="text-small">查看全部</router-link>
          </div>
          <StateBlock
            v-if="attentionServices.length === 0"
            mode="empty"
            message="当前没有异常服务，监控与应用状态均正常"
          />
          <div v-else class="attention-list">
            <div v-for="service in attentionServices" :key="service.service_id" class="attention-row">
              <div class="attention-main">
                <div class="attention-name">
                  <router-link :to="`/services/${service.service_id}`">{{ service.name }}</router-link>
                  <span class="attention-key">{{ service.service_key }}</span>
                </div>
                <div class="attention-meta">
                  <DualStatus :monitor-status="service.monitor_status" :app-status="service.app_status" inline />
                  <span :title="tooltipDateTime(service.last_check_at)">
                    最近观测 {{ formatLastObservation(service.last_check_at) }}
                  </span>
                </div>
              </div>
              <div class="attention-action">
                <router-link
                  v-if="service.active_episode_id"
                  :to="`/episodes/${service.active_episode_id}`"
                  class="active-mark"
                >
                  <span class="status-dot" />
                  进行中的故障
                </router-link>
                <StatusTag v-else status="UNKNOWN" label="暂无进行中故障" tone="neutral" size="small" />
              </div>
            </div>
          </div>
        </section>

        <section class="af-panel">
          <div class="af-panel-head">
            <div class="af-panel-title">
              最近故障
              <span class="af-panel-sub">按开始时间倒序</span>
            </div>
            <router-link to="/episodes" class="text-small">全部记录</router-link>
          </div>
          <StateBlock
            v-if="summary.recent_episodes.length === 0"
            mode="empty"
            message="最近还没有故障记录"
          />
          <div v-else class="recent-list">
            <div
              v-for="episode in summary.recent_episodes.slice(0, 8)"
              :key="episode.episode_id"
              class="recent-row"
              @click="openEpisode(episode.episode_id)"
            >
              <div class="recent-main">
                <div class="recent-name">
                  <span>{{ episode.service_name }}</span>
                  <StatusTag :status="episode.status" size="small" />
                </div>
                <div class="recent-meta">
                  <span :title="tooltipDateTime(episode.started_at)">
                    {{ formatDateTime(episode.started_at) }}
                  </span>
                  <span>{{ diagnosisState(episode.active_task_status, episode.active_task_type).label }}</span>
                </div>
              </div>
              <div class="recent-side">
                <div class="duration">{{ recentDuration(episode.started_at, episode.resolved_at) }}</div>
                <div>{{ statusLabel(episode.status) }}</div>
              </div>
            </div>
          </div>
        </section>
      </div>

      <section class="af-panel">
        <div class="af-panel-head">
          <div class="af-panel-title">
            观测趋势
            <span class="af-panel-sub">监控采样结果随时间的变化</span>
          </div>
        </div>
        <div v-if="!hasTrend" class="chart-empty">窗口内没有采样</div>
        <div v-else ref="chartEl" class="chart-box" />
      </section>
    </template>
  </div>
</template>
