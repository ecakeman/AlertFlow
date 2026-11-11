<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteTarget, fetchServiceTargets, fetchServices } from '../api/services'
import { errorMessage } from '../api/client'
import type { ServiceSummary, Target } from '../api/types'
import ErrorRetry from '../components/ErrorRetry.vue'
import StatusTag from '../components/StatusTag.vue'
import TargetFormDialog from '../components/TargetFormDialog.vue'
import { formatDateTime, localTimeZoneLabel } from '../utils/time'
import { targetSummary, targetTypeLabel, targetTypeTagType } from '../utils/status'

const route = useRoute()
const router = useRouter()

const timezone = localTimeZoneLabel()

const serviceOptions = ref<ServiceSummary[]>([])
const selectedServiceId = ref<string>('')
const targets = ref<Target[]>([])
const loadingServices = ref(false)
const loadingTargets = ref(false)
const error = ref('')
const deletingId = ref('')
const dialogVisible = ref(false)
const editingTarget = ref<Target | null>(null)

const selectedService = computed(
  () => serviceOptions.value.find((item) => item.service_id === selectedServiceId.value) ?? null
)

async function loadServices(): Promise<void> {
  loadingServices.value = true
  try {
    const data = await fetchServices({ limit: 200, offset: 0 })
    serviceOptions.value = data.services ?? []
    if (!selectedServiceId.value && serviceOptions.value.length > 0) {
      const fromQuery = String(route.query.service_id ?? '')
      const matched = serviceOptions.value.find((item) => item.service_id === fromQuery)
      selectedServiceId.value = matched ? matched.service_id : serviceOptions.value[0].service_id
    }
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loadingServices.value = false
  }
}

async function loadTargets(): Promise<void> {
  if (!selectedServiceId.value) {
    targets.value = []
    return
  }
  loadingTargets.value = true
  error.value = ''
  try {
    targets.value = await fetchServiceTargets(selectedServiceId.value)
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    loadingTargets.value = false
  }
}

async function load(): Promise<void> {
  await loadServices()
  await loadTargets()
}

function onServiceChange(value: string): void {
  selectedServiceId.value = value
  router.replace({ name: 'targets', query: value ? { service_id: value } : {} })
  loadTargets()
}

function openCreate(): void {
  if (!selectedServiceId.value) {
    ElMessage.warning('请先选择服务')
    return
  }
  editingTarget.value = null
  dialogVisible.value = true
}

function openEdit(row: Target): void {
  editingTarget.value = row
  dialogVisible.value = true
}

function onSaved(): void {
  dialogVisible.value = false
  loadTargets()
}

function openServiceDetail(): void {
  if (!selectedService.value) return
  router.push({ name: 'service-detail', params: { serviceId: selectedService.value.service_id } })
}

async function removeTarget(row: Target): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `确认删除监控目标「${row.target_name || row.target_key}」？删除后该目标将被软删除并停止采样，历史采样点保留；之后可用相同标识新建目标。`,
      '删除监控目标',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }
    )
  } catch {
    return
  }
  deletingId.value = row.target_id
  try {
    await deleteTarget(row.target_id)
    ElMessage.success('监控目标已删除（历史采样点保留）')
    await loadTargets()
  } catch (err) {
    ElMessage.error(errorMessage(err))
  } finally {
    deletingId.value = ''
  }
}

onMounted(async () => {
  await loadServices()
  await loadTargets()
})

watch(
  () => route.query.service_id,
  (value) => {
    const next = String(value ?? '')
    if (next && next !== selectedServiceId.value) {
      selectedServiceId.value = next
      loadTargets()
    }
  }
)
</script>

<template>
  <div>
    <div class="page-toolbar">
      <el-select
        v-model="selectedServiceId"
        filterable
        placeholder="请选择服务"
        style="width: 280px"
        :loading="loadingServices"
        @change="onServiceChange"
      >
        <el-option
          v-for="item in serviceOptions"
          :key="item.service_id"
          :label="`${item.name}（${item.service_key}）`"
          :value="item.service_id"
        />
      </el-select>
      <el-button v-if="selectedService" link type="primary" @click="openServiceDetail">服务详情</el-button>
      <el-button :loading="loadingTargets" @click="loadTargets">刷新</el-button>
      <div class="toolbar-right">
        <el-button type="primary" :disabled="!selectedServiceId" @click="openCreate">新增目标</el-button>
      </div>
    </div>

    <ErrorRetry v-if="error" :message="error" :loading="loadingTargets || loadingServices" class="section-gap" @retry="load" />

    <el-card shadow="never">
      <template #header>
        <div class="card-header">
          <span class="card-header-title">
            监控目标
            <span class="card-header-sub" v-if="selectedService">
              {{ selectedService.name }} · 共 {{ targets.length }} 个
            </span>
          </span>
          <span class="card-header-sub">时间均为 {{ timezone }} 本地时间；K8s 目标 namespace 继承系统配置</span>
        </div>
      </template>

      <div v-if="!selectedServiceId" class="text-muted">请先选择一个服务，再查看和管理其监控目标。</div>

      <template v-else>
        <el-table v-loading="loadingTargets" :data="targets" size="small" row-key="target_id">
          <el-table-column label="目标" min-width="200">
            <template #default="{ row }">
              <div class="mono">{{ row.target_key }}</div>
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
          <el-table-column label="摘要" min-width="230" show-overflow-tooltip>
            <template #default="{ row }">
              <span class="mono">{{ targetSummary(row) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <StatusTag :status="row.status" />
              <el-tag v-if="!row.enabled" size="small" type="info" effect="plain" class="disabled-tag">停用</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="consecutive_failures" label="连续失败" width="90" align="right" />
          <el-table-column prop="consecutive_successes" label="连续成功" width="90" align="right" />
          <el-table-column label="最近观测时间" width="170">
            <template #default="{ row }">{{ formatDateTime(row.last_check_at) }}</template>
          </el-table-column>
          <el-table-column label="采样/超时(ms)" width="130">
            <template #default="{ row }">
              <span class="mono">{{ row.interval_ms }} / {{ row.timeout_ms }}</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="120" fixed="right">
            <template #default="{ row }">
              <el-button link type="primary" size="small" @click="openEdit(row)">编辑</el-button>
              <el-button
                link
                type="danger"
                size="small"
                :loading="deletingId === row.target_id"
                @click="removeTarget(row)"
              >
                删除
              </el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="该服务暂无监控目标" :image-size="64" class="empty-compact">
              <el-button size="small" type="primary" @click="openCreate">新增目标</el-button>
            </el-empty>
          </template>
        </el-table>
      </template>
    </el-card>

    <TargetFormDialog
      v-model="dialogVisible"
      :service-id="selectedServiceId"
      :service-name="selectedService?.name"
      :target="editingTarget"
      @saved="onSaved"
    />
  </div>
</template>

<style scoped>
.disabled-tag {
  margin-left: 4px;
}
</style>
