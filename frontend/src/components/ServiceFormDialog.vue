<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import type { ResourceKind, ServiceSummary, ServiceView, TargetType } from '../api/types'
import { createService, patchService } from '../api/services'
import { errorMessage } from '../api/client'
import {
  HTTP_ADDRESS_PATTERN,
  K8S_RESOURCE_NAME_PATTERN,
  RESOURCE_KIND_OPTIONS,
  TARGET_DEFAULTS,
  TARGET_KEY_PATTERN,
  TARGET_TYPE_OPTIONS,
  TCP_ADDRESS_PATTERN,
  buildTargetAddress,
  buildTargetConfig
} from '../utils/target'

export type ServiceFormMode = 'create' | 'edit'

const props = defineProps<{
  modelValue: boolean
  mode: ServiceFormMode
  service?: ServiceView | ServiceSummary | null
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'saved', service: ServiceView): void
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (value: boolean) => emit('update:modelValue', value)
})

const isCreate = computed(() => props.mode === 'create')
const editingServiceId = computed(() => (isCreate.value ? '' : props.service?.service_id ?? ''))
const isKubernetes = computed(() => form.target_type === 'KUBERNETES')

interface ServiceFormState {
  service_key: string
  name: string
  description: string
  authority_producer_id: string
  enabled: boolean
  // 初始监控目标（仅新增时使用）
  target_key: string
  target_name: string
  target_type: TargetType
  address: string
  resource_kind: ResourceKind
  resource_name: string
  interval_ms: number
  timeout_ms: number
  failure_threshold: number
  recovery_threshold: number
  is_required: boolean
}

function defaultForm(): ServiceFormState {
  return {
    service_key: '',
    name: '',
    description: '',
    authority_producer_id: '',
    enabled: true,
    target_key: '',
    target_name: '',
    target_type: 'HTTP',
    address: '',
    resource_kind: 'Deployment',
    resource_name: '',
    ...TARGET_DEFAULTS,
    is_required: true
  }
}

const form = reactive<ServiceFormState>(defaultForm())
const formRef = ref<FormInstance>()
const submitting = ref(false)
const submitError = ref('')

const rules = computed<FormRules>(() => {
  const base: FormRules = {
    name: [
      { required: true, message: '请输入服务名称', trigger: 'blur' },
      { max: 200, message: '不超过 200 个字符', trigger: 'blur' }
    ],
    description: [{ max: 500, message: '不超过 500 个字符', trigger: 'blur' }]
  }
  if (!isCreate.value) return base

  return {
    ...base,
    service_key: [
      { required: true, message: '请输入服务标识', trigger: 'blur' },
      { pattern: TARGET_KEY_PATTERN, message: '仅允许字母/数字/点/下划线/短横线，最长 100', trigger: 'blur' }
    ],
    authority_producer_id: [{ required: true, message: '请输入权威应用（authority_producer_id）', trigger: 'blur' }],
    target_key: [
      { required: true, message: '请输入初始目标标识', trigger: 'blur' },
      { pattern: TARGET_KEY_PATTERN, message: '仅允许字母/数字/点/下划线/短横线，最长 100', trigger: 'blur' }
    ],
    target_name: [
      { required: true, message: '请输入初始目标名称', trigger: 'blur' },
      { max: 200, message: '不超过 200 个字符', trigger: 'blur' }
    ],
    address: isKubernetes.value
      ? []
      : [
          { required: true, message: '请输入初始目标地址', trigger: 'blur' },
          form.target_type === 'HTTP'
            ? { pattern: HTTP_ADDRESS_PATTERN, message: 'HTTP 地址需为完整 URL（http:// 或 https://）', trigger: 'blur' }
            : { pattern: TCP_ADDRESS_PATTERN, message: 'TCP 地址需为 host:port', trigger: 'blur' }
        ],
    resource_kind: isKubernetes.value ? [{ required: true, message: '请选择资源类型', trigger: 'change' }] : [],
    resource_name: isKubernetes.value
      ? [
          { required: true, message: '请输入资源名称', trigger: 'blur' },
          { pattern: K8S_RESOURCE_NAME_PATTERN, message: 'K8s 资源名需为小写字母/数字/短横线', trigger: 'blur' }
        ]
      : [],
    interval_ms: [{ required: true, type: 'number', min: 1000, message: '采样间隔不小于 1000ms', trigger: 'blur' }],
    timeout_ms: [{ required: true, type: 'number', min: 500, message: '超时时间不小于 500ms', trigger: 'blur' }],
    failure_threshold: [{ required: true, type: 'number', min: 1, message: '阈值必须大于 0', trigger: 'blur' }],
    recovery_threshold: [{ required: true, type: 'number', min: 1, message: '阈值必须大于 0', trigger: 'blur' }]
  }
})

function resetForm(): void {
  Object.assign(form, defaultForm())
  const service = props.service
  if (service && !isCreate.value) {
    form.service_key = service.service_key
    form.name = service.name
    form.description = service.description ?? ''
    form.enabled = service.enabled
  }
  submitError.value = ''
  formRef.value?.clearValidate()
}

watch(
  () => props.modelValue,
  (open) => {
    if (open) resetForm()
  }
)

function onTargetTypeChange(): void {
  if (form.target_type === 'KUBERNETES') {
    form.address = ''
  } else {
    form.resource_name = ''
  }
  formRef.value?.clearValidate(['address', 'resource_kind', 'resource_name'])
}

async function handleSubmit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  submitError.value = ''
  try {
    if (isCreate.value) {
      const created = await createService({
        service_key: form.service_key.trim(),
        name: form.name.trim(),
        description: form.description.trim(),
        authority_producer_id: form.authority_producer_id.trim(),
        targets: [
          {
            target_key: form.target_key.trim(),
            target_name: form.target_name.trim(),
            target_type: form.target_type,
            address: buildTargetAddress(form.target_type, form.address),
            config: buildTargetConfig(form.target_type, form.resource_kind, form.resource_name),
            interval_ms: form.interval_ms,
            timeout_ms: form.timeout_ms,
            failure_threshold: form.failure_threshold,
            recovery_threshold: form.recovery_threshold,
            is_required: form.is_required,
            enabled: true
          }
        ]
      })
      ElMessage.success('服务已创建')
      emit('saved', created)
    } else {
      if (!editingServiceId.value) throw new Error('缺少服务 ID，无法更新')
      const updated = await patchService(editingServiceId.value, {
        name: form.name.trim(),
        description: form.description.trim(),
        enabled: form.enabled
      })
      ElMessage.success('服务已更新')
      emit('saved', updated)
    }
    visible.value = false
  } catch (error) {
    submitError.value = errorMessage(error)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <el-dialog
    v-model="visible"
    :title="isCreate ? '新增服务' : '编辑服务'"
    width="700px"
    :close-on-click-modal="false"
    destroy-on-close
  >
    <el-alert v-if="submitError" type="error" :closable="false" show-icon class="dialog-error">
      <template #title>保存失败</template>
      {{ submitError }}
    </el-alert>

    <el-form ref="formRef" :model="form" :rules="rules" label-width="132px" @submit.prevent>
      <el-form-item label="服务标识" prop="service_key">
        <el-input v-model="form.service_key" :disabled="!isCreate" maxlength="100" placeholder="例如 alertflow-backend" />
        <div class="form-hint">全局唯一；创建后不可修改。</div>
      </el-form-item>

      <el-form-item label="服务名称" prop="name">
        <el-input v-model="form.name" maxlength="200" placeholder="展示用的服务名称" />
      </el-form-item>

      <el-form-item label="描述" prop="description">
        <el-input v-model="form.description" type="textarea" :rows="2" maxlength="500" show-word-limit placeholder="可选" />
      </el-form-item>

      <el-form-item v-if="isCreate" label="权威应用" prop="authority_producer_id">
        <el-input v-model="form.authority_producer_id" maxlength="100" placeholder="例如 alertflow-backend" />
        <div class="form-hint">该服务对应的应用事件生产者 ID（authority_producer_id），必填。</div>
      </el-form-item>

      <el-form-item v-else label="启用">
        <el-switch v-model="form.enabled" />
        <div class="form-hint">停用后服务与目标停止采样，历史故障与诊断产物保留。</div>
      </el-form-item>

      <el-divider v-if="isCreate" content-position="left">初始监控目标（必填，至少一个）</el-divider>

      <template v-if="isCreate">
        <div class="form-grid">
          <el-form-item label="目标标识" prop="target_key">
            <el-input v-model="form.target_key" maxlength="100" placeholder="例如 http-api" />
          </el-form-item>
          <el-form-item label="目标名称" prop="target_name">
            <el-input v-model="form.target_name" maxlength="200" placeholder="例如 API 健康检查" />
          </el-form-item>
        </div>

        <el-form-item label="目标类型" prop="target_type">
          <el-select v-model="form.target_type" style="width: 220px" @change="onTargetTypeChange">
            <el-option v-for="item in TARGET_TYPE_OPTIONS" :key="item.value" :label="item.label" :value="item.value" />
          </el-select>
        </el-form-item>

        <template v-if="!isKubernetes">
          <el-form-item label="目标地址" prop="address">
            <el-input
              v-model="form.address"
              :placeholder="form.target_type === 'HTTP' ? 'https://example.com/health' : '10.0.0.8:5432'"
            />
          </el-form-item>
        </template>

        <template v-else>
          <div class="form-grid">
            <el-form-item label="资源类型" prop="resource_kind">
              <el-select v-model="form.resource_kind" style="width: 100%">
                <el-option v-for="item in RESOURCE_KIND_OPTIONS" :key="item.value" :label="item.label" :value="item.value" />
              </el-select>
            </el-form-item>
            <el-form-item label="资源名称" prop="resource_name">
              <el-input v-model="form.resource_name" maxlength="253" placeholder="例如 alertflow-backend" />
            </el-form-item>
          </div>
          <div class="form-hint k8s-hint">K8s 目标 namespace 继承系统配置，无需填写。</div>
        </template>

        <div class="form-grid">
          <el-form-item label="采样间隔(ms)" prop="interval_ms">
            <el-input-number v-model="form.interval_ms" :min="1000" :step="1000" :precision="0" style="width: 100%" />
          </el-form-item>
          <el-form-item label="超时(ms)" prop="timeout_ms">
            <el-input-number v-model="form.timeout_ms" :min="500" :step="500" :precision="0" style="width: 100%" />
          </el-form-item>
        </div>

        <div class="form-grid">
          <el-form-item label="连续失败阈值" prop="failure_threshold">
            <el-input-number v-model="form.failure_threshold" :min="1" :precision="0" style="width: 100%" />
          </el-form-item>
          <el-form-item label="连续恢复阈值" prop="recovery_threshold">
            <el-input-number v-model="form.recovery_threshold" :min="1" :precision="0" style="width: 100%" />
          </el-form-item>
        </div>

        <el-form-item label="必需目标">
          <el-switch v-model="form.is_required" />
          <div class="form-hint">必需目标参与服务聚合状态计算。</div>
        </el-form-item>
      </template>
    </el-form>

    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="submitting" @click="handleSubmit">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.k8s-hint {
  margin: -6px 0 12px 132px;
}
</style>
