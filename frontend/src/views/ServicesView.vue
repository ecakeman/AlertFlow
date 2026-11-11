<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteService, fetchServices } from '../api/services'
import { errorMessage } from '../api/client'
import type { ServiceSummary } from '../api/types'
import ErrorRetry from '../components/ErrorRetry.vue'
import ServiceFormDialog from '../components/ServiceFormDialog.vue'
import StatusTag from '../components/StatusTag.vue'
import { formatDateTime } from '../utils/time'

const router = useRouter()

const services = ref<ServiceSummary[]>([])
const total = ref(0)
const loading = ref(false)
const error = ref('')
const statusFilter = ref<'' | 'NORMAL' | 'ABNORMAL' | 'UNKNOWN'>('')
const limit = ref(50)
const offset = ref(0)
const deletingId = ref('')

const dialogVisible = ref(false)
const dialogMode = ref<'create' | 'edit'>('create')
const editingService = ref<ServiceSummary | null>(null)

const page = computed(() => Math.floor(offset.value / limit.value) + 1)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const data = await fetchServices({
      limit: limit.value,
      offset: offset.value,
      status: statusFilter.value
    })
    services.value = data.services ?? []
    total.value = data.total ?? 0
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function onStatusChange(): void {
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

function openCreate(): void {
  dialogMode.value = 'create'
  editingService.value = null
  dialogVisible.value = true
}

function openEdit(row: ServiceSummary): void {
  dialogMode.value = 'edit'
  editingService.value = row
  dialogVisible.value = true
}

function onSaved(): void {
  dialogVisible.value = false
  load()
}

function openDetail(row: ServiceSummary): void {
  router.push({ name: 'service-detail', params: { serviceId: row.service_id } })
}

function openTargets(row: ServiceSummary): void {
  router.push({ name: 'targets', query: { service_id: row.service_id } })
}

function openEpisode(row: ServiceSummary): void {
  if (!row.active_episode_id) return
  router.push({ name: 'episode-detail', params: { episodeId: row.active_episode_id } })
}

async function removeService(row: ServiceSummary): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `确认删除服务「${row.name}」？删除后服务与监控目标将被软删除并停止采样，历史故障 Episode、采样点与诊断产物会保留。`,
      '删除服务',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }
    )
  } catch {
    return
  }
  deletingId.value = row.service_id
  try {
    await deleteService(row.service_id)
    ElMessage.success('服务已删除（历史故障与诊断产物保留）')
    if (services.value.length === 1 && offset.value > 0) {
      offset.value = Math.max(0, offset.value - limit.value)
    }
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err))
  } finally {
    deletingId.value = ''
  }
}

onMounted(load)
</script>

<template>
  <div>
    <div class="page-toolbar">
      <el-select v-model="statusFilter" placeholder="全部状态" style="width: 160px" @change="onStatusChange">
        <el-option label="全部状态" value="" />
        <el-option label="正常 (NORMAL)" value="NORMAL" />
        <el-option label="异常 (ABNORMAL)" value="ABNORMAL" />
        <el-option label="未知 (UNKNOWN)" value="UNKNOWN" />
      </el-select>
      <el-button :loading="loading" @click="load">刷新</el-button>
      <div class="toolbar-right">
        <el-button type="primary" @click="openCreate">新增服务</el-button>
      </div>
    </div>

    <ErrorRetry v-if="error" :message="error" :loading="loading" class="section-gap" @retry="load" />

    <el-card shadow="never">
      <el-table v-loading="loading" :data="services" size="small" row-key="service_id">
        <el-table-column label="服务名称" min-width="220">
          <template #default="{ row }">
            <div class="service-name">{{ row.name }}</div>
            <div class="mono text-muted">{{ row.service_key }}</div>
          </template>
        </el-table-column>
        <el-table-column label="描述" min-width="180" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="text-secondary">{{ row.description || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="监控状态" width="150">
          <template #default="{ row }">
            <div class="status-cell">
              <StatusTag :status="row.monitor_status" />
              <el-tag v-if="!row.enabled" size="small" type="info" effect="plain">已禁用</el-tag>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="应用状态" width="100">
          <template #default="{ row }">
            <StatusTag :status="row.app_status" />
          </template>
        </el-table-column>
        <el-table-column prop="target_count" label="目标数" width="80" align="right" />
        <el-table-column label="最近观测时间" width="170">
          <template #default="{ row }">{{ formatDateTime(row.last_check_at) }}</template>
        </el-table-column>
        <el-table-column label="活动 Episode" width="130">
          <template #default="{ row }">
            <span v-if="row.active_episode_id" class="mono clickable" @click="openEpisode(row)">
              {{ row.active_episode_id.slice(0, 8) }}
            </span>
            <span v-else class="text-muted">—</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="220" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="openDetail(row)">详情</el-button>
            <el-button link type="primary" size="small" @click="openEdit(row)">编辑</el-button>
            <el-button link type="primary" size="small" @click="openTargets(row)">目标</el-button>
            <el-button
              link
              type="danger"
              size="small"
              :loading="deletingId === row.service_id"
              @click="removeService(row)"
            >
              删除
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无服务" :image-size="72" class="empty-compact">
            <el-button type="primary" @click="openCreate">新增服务</el-button>
          </el-empty>
        </template>
      </el-table>

      <div class="pagination-bar">
        <el-pagination
          :current-page="page"
          :page-size="limit"
          :total="total"
          :page-sizes="[20, 50, 100, 200]"
          layout="total, sizes, prev, pager, next, jumper"
          background
          @current-change="onPageChange"
          @size-change="onSizeChange"
        />
      </div>
    </el-card>

    <ServiceFormDialog
      v-model="dialogVisible"
      :mode="dialogMode"
      :service="editingService"
      @saved="onSaved"
    />
  </div>
</template>

<style scoped>
.service-name {
  font-weight: 500;
}

.status-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.pagination-bar {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
</style>
