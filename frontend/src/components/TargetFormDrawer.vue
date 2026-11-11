<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { createTarget, patchTarget } from '../api/services'
import { errorMessage } from '../api/client'
import type { ResourceKind, Target, TargetType } from '../api/types'
import {
  HTTP_ADDRESS_PATTERN,
  RESOURCE_KIND_OPTIONS,
  TARGET_DEFAULTS,
  TARGET_KEY_PATTERN,
  TARGET_TYPE_OPTIONS,
  TCP_ADDRESS_PATTERN,
  buildTargetAddress,
  buildTargetConfig,
  validateK8sResourceName
} from '../utils/target'

const props = defineProps<{
  modelValue: boolean
  serviceId: string
  target?: Target | null
}>()

const emit = defineEmits<{
  (event: 'update:modelValue', value: boolean): void
  (event: 'saved'): void
}>()

const isEdit = computed(() => Boolean(props.target?.target_id))
const formRef = ref<FormInstance>()
const submitting = ref(false)
const errorMsg = ref('')

interface TargetFormModel {
  target_key: string
  target_name: string
  target_type: TargetType
  address: string
  resource_kind: ResourceKind
  resource_name: string
  interval_seconds: number
  timeout_seconds: number
  failure_threshold: number
  recovery_threshold: number
  is_required: boolean
  enabled: boolean
}

const form = reactive<TargetFormModel>({
  target_key: '',
  target_name: '',
  target_type: 'HTTP',
  address: '',
  resource_kind: 'Deployment',
  resource_name: '',
  interval_seconds: TARGET_DEFAULTS.interval_ms / 1000,
  timeout_seconds: TARGET_DEFAULTS.timeout_ms / 1000,
  failure_threshold: TARGET_DEFAULTS.failure_threshold,
  recovery_threshold: TARGET_DEFAULTS.recovery_threshold,
  is_required: true,
  enabled: true
})

const isK8s = computed(() => form.target_type === 'KUBERNETES')

const rules = computed<FormRules>(() => {
  const result: FormRules = {
    target_name: [{ required: true, message: '请填写监控源名称', trigger: 'blur' }],
    interval_seconds: [{ required: true, message: '请填写采样间隔', trigger: 'blur' }],
    timeout_seconds: [{ required: true, message: '请填写超时时间', trigger: 'blur' }]
  }
  if (!isEdit.value) {
    result.target_key = [
      { required: true, message: '请填写监控源标识', trigger: 'blur' },
      {
        validator: (_rule, value: string, callback) => {
          if (value && !TARGET_KEY_PATTERN.test(value.trim())) {
            callback(new Error('仅支持字母、数字、点、下划线和短横线，最长 100 字符'))
            return
          }
          callback()
        },
        trigger: 'blur'
      }
    ]
  }
  if (isK8s.value) {
    result.resource_name = [
      { required: true, message: '请填写 K8s 资源名', trigger: 'blur' },
      {
        validator: (_rule, value: string, callback) => {
          const message = validateK8sResourceName(value || '')
          callback(message ? new Error(message) : undefined)
        },
        trigger: 'blur'
      }
    ]
  } else {
    result.address = [
      { required: true, message: '请填写监控地址', trigger: 'blur' },
      {
        validator: (_rule, value: string, callback) => {
          const text = (value || '').trim()
          const pattern = form.target_type === 'TCP' ? TCP_ADDRESS_PATTERN : HTTP_ADDRESS_PATTERN
          if (text && !pattern.test(text)) {
            callback(
              new Error(form.target_type === 'TCP' ? '格式应为 host:port，例如 db:5432' : '格式应为 http(s):// 地址')
            )
            return
          }
          callback()
        },
        trigger: 'blur'
      }
    ]
  }
  return result
})

watch(
  () => props.modelValue,
  (visible) => {
    if (!visible) return
    errorMsg.value = ''
    formRef.value?.clearValidate()
    const target = props.target
    if (target) {
      form.target_key = target.target_key
      form.target_name = target.target_name
      form.target_type = target.target_type
      form.address = target.address
      form.resource_kind = (target.config?.resource_kind as ResourceKind) || 'Deployment'
      form.resource_name = String(target.config?.resource_name ?? '')
      form.interval_seconds = target.interval_ms / 1000
      form.timeout_seconds = target.timeout_ms / 1000
      form.failure_threshold = target.failure_threshold
      form.recovery_threshold = target.recovery_threshold
      form.is_required = target.is_required
      form.enabled = target.enabled
      return
    }
    form.target_key = ''
    form.target_name = ''
    form.target_type = 'HTTP'
    form.address = ''
    form.resource_kind = 'Deployment'
    form.resource_name = ''
    form.interval_seconds = TARGET_DEFAULTS.interval_ms / 1000
    form.timeout_seconds = TARGET_DEFAULTS.timeout_ms / 1000
    form.failure_threshold = TARGET_DEFAULTS.failure_threshold
    form.recovery_threshold = TARGET_DEFAULTS.recovery_threshold
    form.is_required = true
    form.enabled = true
  },
  { immediate: true }
)

function close() {
  emit('update:modelValue', false)
}

async function submit() {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  errorMsg.value = ''
  const payload = {
    target_name: form.target_name.trim(),
    address: buildTargetAddress(form.target_type, form.address),
    config: buildTargetConfig(form.target_type, form.resource_kind, form.resource_name),
    interval_ms: Math.round(form.interval_seconds * 1000),
    timeout_ms: Math.round(form.timeout_seconds * 1000),
    failure_threshold: form.failure_threshold,
    recovery_threshold: form.recovery_threshold,
    is_required: form.is_required,
    enabled: form.enabled
  }

  try {
    if (props.target) {
      await patchTarget(props.target.target_id, payload)
      ElMessage.success('监控源已更新')
    } else {
      await createTarget(props.serviceId, {
        target_key: form.target_key.trim(),
        target_type: form.target_type,
        ...payload
      })
      ElMessage.success('监控源已新增')
    }
    emit('saved')
    close()
  } catch (error) {
    errorMsg.value = errorMessage(error)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <el-drawer
    :model-value="modelValue"
    :title="isEdit ? '编辑监控源' : '新增监控源'"
    size="520px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-alert v-if="errorMsg" class="dialog-error" type="error" :closable="false" :title="errorMsg" />

    <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
      <div class="form-grid">
        <el-form-item label="监控源标识" prop="target_key">
          <el-input v-model="form.target_key" :disabled="isEdit" maxlength="100" placeholder="例如 http-probe" />
        </el-form-item>
        <el-form-item label="监控源名称" prop="target_name">
          <el-input v-model="form.target_name" maxlength="100" placeholder="例如 订单接口" />
        </el-form-item>
      </div>

      <el-form-item label="类型">
        <el-radio-group v-model="form.target_type" :disabled="isEdit">
          <el-radio-button v-for="option in TARGET_TYPE_OPTIONS" :key="option.value" :value="option.value">
            {{ option.label }}
          </el-radio-button>
        </el-radio-group>
        <div v-if="isEdit" class="form-hint">监控类型创建后不可修改，如需切换请新增一个监控源。</div>
      </el-form-item>

      <div v-if="isK8s" class="form-grid">
        <el-form-item label="资源类型">
          <el-select v-model="form.resource_kind" style="width: 100%">
            <el-option v-for="option in RESOURCE_KIND_OPTIONS" :key="option.value" :label="option.label" :value="option.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="资源名" prop="resource_name">
          <el-input v-model="form.resource_name" placeholder="例如 order-service" />
        </el-form-item>
      </div>
      <el-form-item v-else label="监控地址" prop="address">
        <el-input
          v-model="form.address"
          :placeholder="form.target_type === 'TCP' ? 'host:port，例如 db:5432' : 'https://example.com/health'"
        />
      </el-form-item>
      <div v-if="isK8s" class="form-hint" style="margin: -8px 0 12px">
        namespace 继承服务端配置，不需要为每个监控源单独填写。
      </div>

      <el-form-item label="启用监控">
        <el-switch v-model="form.enabled" />
        <span class="form-inline-hint">关闭后该监控源暂停采样，但历史采样点与故障记录会保留。</span>
      </el-form-item>

      <div class="drawer-section-title">采样与判定</div>
      <div class="form-grid form-grid--3">
        <el-form-item label="间隔（秒）" prop="interval_seconds">
          <el-input-number v-model="form.interval_seconds" :min="1" :max="3600" :step="1" style="width: 100%" />
        </el-form-item>
        <el-form-item label="超时（秒）" prop="timeout_seconds">
          <el-input-number v-model="form.timeout_seconds" :min="0.5" :max="600" :step="0.5" style="width: 100%" />
        </el-form-item>
        <el-form-item label="必需监控源">
          <el-switch v-model="form.is_required" />
        </el-form-item>
        <el-form-item label="失败阈值（次）">
          <el-input-number v-model="form.failure_threshold" :min="1" :max="100" style="width: 100%" />
        </el-form-item>
        <el-form-item label="恢复阈值（次）">
          <el-input-number v-model="form.recovery_threshold" :min="1" :max="100" style="width: 100%" />
        </el-form-item>
      </div>
    </el-form>

    <template #footer>
      <el-button @click="close">取消</el-button>
      <el-button type="primary" :loading="submitting" @click="submit">
        {{ isEdit ? '保存' : '新增监控源' }}
      </el-button>
    </template>
  </el-drawer>
</template>
