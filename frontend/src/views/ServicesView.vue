<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteService, fetchService, fetchServices } from '../api/services'
import { errorMessage } from '../api/client'
import type { MonitorStatus, ServiceSummary, ServiceView } from '../api/types'
import PageHeader from '../components/PageHeader.vue'
import StateBlock from '../components/StateBlock.vue'
import StatusTag from '../components/StatusTag.vue'
import DualStatus from '../components/DualStatus.vue'
import ServiceFormDialog from '../components/ServiceFormDialog.vue'
import { formatLastObservation, tooltipDateTime } from '../utils/time'

const route = useRoute()
const router = useRouter()

const services = ref<ServiceSummary[]>([])
const total = ref(0)
const loading = ref(false)
const error = ref('')
const keyword = ref('')
const status = ref<MonitorStatus | ''>('')
const page = ref(1)
const pageSize = ref(20)

const dialogVisible = ref(false)
const editingService = ref<ServiceView | null>(null)

const statusOptions: Array<{ label: string; value: MonitorStatus | '' }> = [
  { label: '全部状态', value: '' },
  { label: '正常', value: 'NORMAL' },
  { label: '异常', value: 'ABNORMAL' },
  { label: '尚未观测', value: 'UNKNOWN' }
]

const offset = computed(() => (page.value - 1) * pageSize.value)

let searchTimer: ReturnType<typeof setTimeout> | undefined

async function load() {
  loading.value = true
  error.value = ''
  try {
    const data = await fetchServices({
      limit: pageSize.value,
      offset: offset.value,
      status: status.value,
      q: keyword.value
    })
    services.value = data.services
    total.value = data.total
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function applySearch() {
  page.value = 1
  syncQuery()
  load()
}

function onKeywordInput() {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(applySearch, 300)
}

function onStatusChange() {
  page.value = 1
  syncQuery()
  load()
}

function syncQuery() {
  const query: Record<string, string> = {}
  if (status.value) query.status = status.value
  if (keyword.value.trim()) query.q = keyword.value.trim()
  router.replace({ query }).catch(() => undefined)
}

function resetFilters() {
  keyword.value = ''
  status.value = ''
  page.value = 1
  syncQuery()
  load()
}

function changePage(next: number) {
  page.value = next
  load()
}

function changePageSize(size: number) {
  pageSize.value = size
  page.value = 1
  load()
}

function openCreate() {
  editingService.value = null
  dialogVisible.value = true
}

async function openEdit(service: ServiceSummary) {
  try {
    editingService.value = await fetchService(service.service_id)
    dialogVisible.value = true
  } catch (err) {
    ElMessage.error(errorMessage(err))
  }
}

async function remove(service: ServiceSummary) {
  try {
    await ElMessageBox.confirm(
      '删除后，该服务不再出现在目录中；历史故障 Episode、采样点与诊断产物都会保留，便于回溯。',
      `删除服务「${service.name}」`,
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }
  try {
    await deleteService(service.service_id)
    ElMessage.success('服务已删除')
    if (services.value.length === 1 && page.value > 1) page.value -= 1
    load()
  } catch (err) {
    ElMessage.error(errorMessage(err))
  }
}

function handleCommand(command: string | number | object, service: ServiceSummary) {
  if (command === 'edit') {
    openEdit(service)
    return
  }
  if (command === 'delete') remove(service)
}

onMounted(() => {
  const queryStatus = String(route.query.status ?? '')
  if (queryStatus === 'NORMAL' || queryStatus === 'ABNORMAL' || queryStatus === 'UNKNOWN') {
    status.value = queryStatus
  }
  keyword.value = String(route.query.q ?? '')
  load()
})

onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
})
</script>

<template>
  <div>
    <PageHeader title="服务" subtitle="服务目录、健康状态与监控源">
      <template #actions>
        <el-button :loading="loading" @click="load">
          <el-icon><Refresh /></el-icon>
          <span>刷新</span>
        </el-button>
        <el-button type="primary" @click="openCreate">
          <el-icon><Plus /></el-icon>
          <span>新增服务</span>
        </el-button>
      </template>
    </PageHeader>

    <div class="toolbar">
      <div class="filter-bar">
        <el-input
          v-model="keyword"
          class="service-search"
          placeholder="搜索服务名称或标识"
          clearable
          @input="onKeywordInput"
          @clear="applySearch"
        >
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-select v-model="status" style="width: 140px" @change="onStatusChange">
          <el-option v-for="option in statusOptions" :key="option.value" :label="option.label" :value="option.value" />
        </el-select>
        <el-button v-if="keyword || status" text @click="resetFilters">清空筛选</el-button>
      </div>
      <div class="text-small text-secondary">共 {{ total }} 个服务</div>
    </div>

    <div class="af-panel">
      <StateBlock v-if="loading && services.length === 0" mode="loading" />
      <StateBlock
        v-else-if="error && services.length === 0"
        mode="error"
        :message="error"
        @retry="load"
      />
      <el-table v-else v-loading="loading" :data="services" row-key="service_id" style="width: 100%">
        <el-table-column label="服务" min-width="230">
          <template #default="{ row }">
            <div class="service-cell">
              <router-link :to="`/services/${row.service_id}`" class="service-name">{{ row.name }}</router-link>
              <StatusTag v-if="!row.enabled" status="DISABLED" size="small" />
              <div class="service-key mono">{{ row.service_key }}</div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="健康" min-width="150">
          <template #default="{ row }">
            <DualStatus :monitor-status="row.monitor_status" :app-status="row.app_status" />
          </template>
        </el-table-column>
        <el-table-column label="监控源" width="120">
          <template #default="{ row }">
            <span class="num">{{ row.target_count }}</span>
            <span class="text-muted text-small"> 个</span>
          </template>
        </el-table-column>
        <el-table-column label="最近观测" min-width="180">
          <template #default="{ row }">
            <span :title="tooltipDateTime(row.last_check_at)" class="text-secondary num">
              {{ formatLastObservation(row.last_check_at) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="活动故障" width="150">
          <template #default="{ row }">
            <router-link v-if="row.active_episode_id" :to="`/episodes/${row.active_episode_id}`" class="active-mark">
              <span class="status-dot" />
              进行中
            </router-link>
            <span v-else class="text-muted">—</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="130" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
              <router-link :to="`/services/${row.service_id}`" class="table-link">打开</router-link>
              <el-dropdown trigger="click" @command="(command: string | number | object) => handleCommand(command, row)">
                <span class="table-link">更多<el-icon><ArrowDown /></el-icon></span>
                <template #dropdown>
                  <el-dropdown-menu>
                    <el-dropdown-item command="edit">编辑服务</el-dropdown-item>
                    <el-dropdown-item command="delete" divided>删除服务</el-dropdown-item>
                  </el-dropdown-menu>
                </template>
              </el-dropdown>
            </div>
          </template>
        </el-table-column>
        <template #empty>
          <StateBlock
            mode="empty"
            :message="keyword || status ? '没有匹配的服务，试试调整搜索或筛选条件' : '还没有服务，先新增一个并配置监控源'"
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

    <ServiceFormDialog v-model="dialogVisible" :service="editingService" @saved="load" />
  </div>
</template>
