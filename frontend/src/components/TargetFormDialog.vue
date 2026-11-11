<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import type { ResourceKind, Target, TargetType } from '../api/types'
import { createTarget, patchTarget, type TargetPatchPayload } from '../api/services'
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
import { configValue } from '../utils/status'

const props = defineProps<{
  modelValue: boolean
  serviceId: string
  serviceName?: string
  /** 传入 target 表示编辑，否则为新增 */
  target?: Target | null
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'saved', target: Target): void
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (value: boolean) => emit('update:modelValue', value)
})

const isEdit = computed(() => Boolean(props.target))
const isKubernetes = computed(() => form.target_type === 'KUBERNETES')

interface TargetFormState {
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
  enabled: boolean
}

function defaultForm(): TargetFormState {
  return {
    target_key: '',
    target_name: '',
    target_type: 'HTTP',
    address: '',
    resource_kind: 'Deployment',
    resource_name: '',
    ...TARGET_DEFAULTS,
    is_required: true,
    enabled: true
  }
}

const form = reactive<TargetFormState>(defaultForm())
const formRef = ref<FormInstance>()
const submitting = ref(false)
const submitError = ref('')

const rules = computed<FormRules>(() => ({
  target_key: isEdit.value
    ? []
    : [
        { required: true, message: '请输入目标标识', trigger: 'blur' },
        { pattern: TARGET_KEY_PATTERN, message: '仅允许字母/数字/点/下划线/短横线，最长 100', trigger: 'blur' }
      ],
  target_name: [
    { required: true, message: '请输入目标名称', trigger: 'blur' },
    { max: 200, message: '不超过 200 个字符', trigger: 'blur' }
  ],
  address: isKubernetes.value
    ? []
    : [
        { required: true, message: '请输入地址', trigger: 'blur' },
        form.target_type === 'HTTP'
          ? { pattern: HTTP_ADDRESS_PATTERN, message: 'HTTP 地址需为完整 URL（http:// 或 https://）', trigger: 'blur' }
          : { pattern: TCP_ADDRESS_PATTERN, message: 'TCP 地址需为 host:port，例如 10.0.0.8:5432', trigger: 'blur' }
      ],
  resource_kind: isKubernetes.value ? [{ required: true, message: '请选择资源类型', trigger: 'change' }] : [],
  resource_name: isKubernetes.value
    ? [
        { required: true, message: '请输入资源名称', trigger: 'blur' },
        { pattern: K8S_RESOURCE_NAME_PATTERN, message: 'K8s 资源名需为小写字母/数字/短横线，且以字母或数字开头结尾', trigger: 'blur' },
        { max: 253, message: '不超过 253 个字符', trigger: 'blur' }
      ]
    : [],
  interval_ms: [
    { required: true, type: 'number', message: '请输入采样间隔', trigger: 'blur' },
    { type: 'number', min: 1000, message: '采样间隔不小于 1000ms', trigger: 'blur' }
  ],
  timeout_ms: [
    { required: true, type: 'number', message: '请输入超时时间', trigger: 'blur' },
    { type: 'number', min: 500, message: '超时时间不小于 500ms', trigger: 'blur' }
  ],
  failure_threshold: [
    { required: true, type: 'number', message: '请输入连续失败阈值', trigger: 'blur' },
    { type: 'number', min: 1, message: '阈值必须大于 0', trigger: 'blur' }
  ],
  recovery_threshold: [
    { required: true, type: 'number', message: '请输入连续恢复阈值', trigger: 'blur' },
    { type: 'number', min: 1, message: '阈值必须大于 0', trigger: 'blur' }
  ]
}))

function resetForm(): void {
  Object.assign(form, defaultForm())
  const target = props.target
  if (target) {
    Object.assign(form, {
      target_key: target.target_key,
      target_name: target.target_name,
      target_type: target.target_type,
      address: target.address ?? '',
      resource_kind: (configValue(target.config, 'resource_kind') || 'Deployment') as ResourceKind,
      resource_name: configValue(target.config, 'resource_name'),
      interval_ms: target.interval_ms,
      timeout_ms: target.timeout_ms,
      failure_threshold: target.failure_threshold,
      recovery_threshold: target.recovery_threshold,
      is_required: target.is_required,
      enabled: target.enabled
    })
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

watch(
  () => props.target,
  () => {
    if (props.modelValue) resetForm()
  }
)

function onTypeChange(): void {
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
    const base = {
      target_name: form.target_name.trim(),
      address: buildTargetAddress(form.target_type, form.address),
      config: buildTargetConfig(form.target_type, form.resource_kind, form.resource_name),
      interval_ms: form.interval_ms,
      timeout_ms: form.timeout_ms,
      failure_threshold: form.failure_threshold,
      recovery_threshold: form.recovery_threshold,
      is_required: form.is_required,
      enabled: form.enabled
    }
    let saved: Target
    if (props.target) {
      const payload: TargetPatchPayload = base
      saved = await patchTarget(props.target.target_id, payload)
      ElMessage.success('监控目标已更新')
    } else {
      saved = await createTarget(props.serviceId, {
        ...base,
        target_key: form.target_key.trim(),
        target_type: form.target_type
      })
      ElMessage.success('监控目标已创建')
    }
    emit('saved', saved)
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
    :title="isEdit ? '编辑监控目标' : '新增监控目标'"
    width="660px"
    :close-on-click-modal="false"
    destroy-on-close
  >
    <div v-if="serviceName" class="dialog-service-hint">
      所属服务：<span class="mono">{{ serviceName }}</span>
    </div>

    <el-alert v-if="submitError" type="error" :closable="false" show-icon class="dialog-error">
      <template #title>保存失败</template>
      {{ submitError }}
    </el-alert>

    <el-form ref="formRef" :model="form" :rules="rules" label-width="112px" @submit.prevent>
      <el-form-item label="目标标识" prop="target_key">
        <el-input
          v-model="form.target_key"
          :disabled="isEdit"
          maxlength="100"
          placeholder="例如 http-api / tcp-db / k8s-deploy"
        />
        <div class="form-hint">同一服务内唯一；编辑时不可修改。K8s 资源名独立填写。</div>
      </el-form-item>

      <el-form-item label="目标名称" prop="target_name">
        <el-input v-model="form.target_name" maxlength="200" placeholder="用于展示的目标名称" />
      </el-form-item>

      <el-form-item label="目标类型" prop="target_type">
        <el-select v-model="form.target_type" :disabled="isEdit" style="width: 100%" @change="onTypeChange">
          <el-option v-for="item in TARGET_TYPE_OPTIONS" :key="item.value" :label="item.label" :value="item.value" />
        </el-select>
        <div v-if="isEdit" class="form-hint">编辑时不允许修改目标类型。</div>
      </el-form-item>

      <template v-if="!isKubernetes">
        <el-form-item label="地址" prop="address">
          <el-input
            v-model="form.address"
            :placeholder="form.target_type === 'HTTP' ? 'https://example.com/health' : '10.0.0.8:5432'"
          />
          <div class="form-hint">
            {{ form.target_type === 'HTTP' ? 'HTTP 探针会 GET 该完整 URL，2xx/3xx 视为成功。' : 'TCP 探针会尝试建立 TCP 连接。' }}
          </div>
        </el-form-item>
      </template>

      <template v-else>
        <el-alert type="info" :closable="false" show-icon class="dialog-error">
          <template #title>K8s 目标</template>
          只需填写资源类型与资源名；namespace 继承系统配置，无需在此填写。
        </el-alert>
        <el-form-item label="资源类型" prop="resource_kind">
          <el-select v-model="form.resource_kind" style="width: 100%">
            <el-option v-for="item in RESOURCE_KIND_OPTIONS" :key="item.value" :label="item.label" :value="item.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="资源名称" prop="resource_name">
          <el-input v-model="form.resource_name" placeholder="例如 alertflow-backend" maxlength="253" />
          <div class="form-hint">仅支持 Deployment / StatefulSet；namespace 由系统配置决定。</div>
        </el-form-item>
      </template>

      <el-form-item label="采样间隔(ms)" prop="interval_ms">
        <el-input-number v-model="form.interval_ms" :min="1000" :step="1000" :precision="0" style="width: 180px" />
        <span class="form-inline-hint">默认 10000</span>
      </el-form-item>

      <el-form-item label="超时(ms)" prop="timeout_ms">
        <el-input-number v-model="form.timeout_ms" :min="500" :step="500" :precision="0" style="width: 180px" />
        <span class="form-inline-hint">默认 3000</span>
      </el-form-item>

      <div class="form-grid">
        <el-form-item label="连续失败阈值" prop="failure_threshold">
          <el-input-number v-model="form.failure_threshold" :min="1" :precision="0" style="width: 100%" />
        </el-form-item>
        <el-form-item label="连续恢复阈值" prop="recovery_threshold">
          <el-input-number v-model="form.recovery_threshold" :min="1" :precision="0" style="width: 100%" />
        </el-form-item>
      </div>

      <div class="form-grid">
        <el-form-item label="必需目标">
          <el-switch v-model="form.is_required" />
          <div class="form-hint">必需目标参与服务聚合状态计算。</div>
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
          <div class="form-hint">停用后停止采样，但不删除历史数据。</div>
        </el-form-item>
      </div>
    </el-form>

    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="submitting" @click="handleSubmit">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.dialog-service-hint {
  color: var(--af-text-secondary);
  font-size: 13px;
  margin-bottom: 10px;
}

.form-inline-hint {
  color: var(--af-text-muted);
  font-size: 12px;
  margin-left: 10px;
}
</style>
