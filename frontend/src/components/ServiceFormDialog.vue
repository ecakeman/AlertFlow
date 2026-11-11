<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { createService, patchService } from '../api/services'
import { errorMessage } from '../api/client'
import type { ResourceKind, ServiceView, TargetType } from '../api/types'
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
  service?: ServiceView | null
}>()

const emit = defineEmits<{
  (event: 'update:modelValue', value: boolean): void
  (event: 'saved'): void
}>()

const isEdit = computed(() => Boolean(props.service?.service_id))
const formRef = ref<FormInstance>()
const submitting = ref(false)
const errorMsg = ref('')

interface ServiceFormModel {
  service_key: string
  name: string
  description: string
  authority_producer_id: string
  enabled: boolean
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
}

const form = reactive<ServiceFormModel>({
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
  interval_seconds: TARGET_DEFAULTS.interval_ms / 1000,
  timeout_seconds: TARGET_DEFAULTS.timeout_ms / 1000,
  failure_threshold: TARGET_DEFAULTS.failure_threshold,
  recovery_threshold: TARGET_DEFAULTS.recovery_threshold,
  is_required: true
})

const isK8s = computed(() => form.target_type === 'KUBERNETES')

const rules = computed<FormRules>(() => {
  const base: FormRules = {
    service_key: [
      { required: true, message: '请填写服务标识（service_key）', trigger: 'blur' },
      {
        validator: (_rule, value: string, callback) => {
          if (!value || !value.trim()) {
            callback(new Error('请填写服务标识（service_key）'))
            return
          }
          if (value.trim().length > 100) {
            callback(new Error('服务标识不能超过 100 个字符'))
            return
          }
          callback()
        },
        trigger: 'blur'
      }
    ],
    name: [{ required: true, message: '请填写服务名称', trigger: 'blur' }],
    authority_producer_id: [{ required: true, message: '请填写应用侧生产者标识', trigger: 'blur' }]
  }

  if (isEdit.value) {
    return { name: base.name }
  }

  const targetRules: FormRules = {
    target_key: [
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
    ],
    target_name: [{ required: true, message: '请填写监控源名称', trigger: 'blur' }],
    interval_seconds: [{ required: true, message: '请填写采样间隔', trigger: 'blur' }],
    timeout_seconds: [{ required: true, message: '请填写超时时间', trigger: 'blur' }]
  }

  if (isK8s.value) {
    targetRules.resource_name = [
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
    targetRules.address = [
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

  return { ...base, ...targetRules }
})

watch(
  () => props.modelValue,
  (visible) => {
    if (!visible) return
    errorMsg.value = ''
    formRef.value?.clearValidate()
    if (props.service) {
      form.service_key = props.service.service_key
      form.name = props.service.name
      form.description = props.service.description
      form.authority_producer_id = props.service.authority_producer_id
      form.enabled = props.service.enabled
      return
    }
    form.service_key = ''
    form.name = ''
    form.description = ''
    form.authority_producer_id = ''
    form.enabled = true
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
  try {
    if (props.service) {
      await patchService(props.service.service_id, {
        name: form.name.trim(),
        description: form.description.trim(),
        enabled: form.enabled
      })
      ElMessage.success('服务信息已更新')
    } else {
      await createService({
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
            interval_ms: Math.round(form.interval_seconds * 1000),
            timeout_ms: Math.round(form.timeout_seconds * 1000),
            failure_threshold: form.failure_threshold,
            recovery_threshold: form.recovery_threshold,
            is_required: form.is_required,
            enabled: true
          }
        ]
      })
      ElMessage.success('服务已创建')
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
  <el-dialog
    :model-value="modelValue"
    :title="isEdit ? '编辑服务' : '新增服务'"
    width="640px"
    :close-on-click-modal="false"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-alert v-if="errorMsg" class="dialog-error" type="error" :closable="false" :title="errorMsg" />

    <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
      <template v-if="!isEdit">
        <div class="form-grid">
          <el-form-item label="服务标识（service_key）" prop="service_key">
            <el-input v-model="form.service_key" maxlength="100" placeholder="例如 order-service" />
          </el-form-item>
          <el-form-item label="服务名称" prop="name">
            <el-input v-model="form.name" maxlength="100" placeholder="例如 订单服务" />
          </el-form-item>
        </div>
        <el-form-item label="应用侧生产者标识" prop="authority_producer_id">
          <el-input v-model="form.authority_producer_id" maxlength="100" placeholder="应用上报事件时使用的 producer_id" />
          <div class="form-hint">应用健康状态以该生产者上报的事件为准；监控状态来自下方监控源。</div>
        </el-form-item>
      </template>
      <template v-else>
        <el-form-item label="服务标识（service_key）">
          <el-input :model-value="form.service_key" disabled />
        </el-form-item>
        <el-form-item label="服务名称" prop="name">
          <el-input v-model="form.name" maxlength="100" />
        </el-form-item>
        <el-form-item label="启用监控">
          <el-switch v-model="form.enabled" />
        </el-form-item>
      </template>

      <el-form-item label="服务说明">
        <el-input
          v-model="form.description"
          type="textarea"
          :rows="2"
          maxlength="500"
          placeholder="这项服务负责什么，便于值班同学快速判断影响面"
        />
      </el-form-item>

      <template v-if="!isEdit">
        <div class="drawer-section-title">初始监控源</div>
        <div class="form-grid">
          <el-form-item label="监控源标识" prop="target_key">
            <el-input v-model="form.target_key" maxlength="100" placeholder="例如 http-probe" />
          </el-form-item>
          <el-form-item label="监控源名称" prop="target_name">
            <el-input v-model="form.target_name" maxlength="100" placeholder="例如 订单接口" />
          </el-form-item>
        </div>
        <el-form-item label="类型">
          <el-radio-group v-model="form.target_type">
            <el-radio-button v-for="option in TARGET_TYPE_OPTIONS" :key="option.value" :value="option.value">
              {{ option.label }}
            </el-radio-button>
          </el-radio-group>
        </el-form-item>

        <div v-if="isK8s" class="form-grid">
          <el-form-item label="资源类型">
            <el-select v-model="form.resource_kind" style="width: 100%">
              <el-option v-for="option in RESOURCE_KIND_OPTIONS" :key="option.value" :label="option.label" :value="option.value" />
            </el-select>
          </el-form-item>
          <el-form-item label="资源名" prop="resource_name">
            <el-input v-model="form.resource_name" placeholder="例如 order-service" />
            <div class="form-hint">namespace 继承服务端配置。</div>
          </el-form-item>
        </div>
        <el-form-item v-else label="监控地址" prop="address">
          <el-input
            v-model="form.address"
            :placeholder="form.target_type === 'TCP' ? 'host:port，例如 db:5432' : 'https://example.com/health'"
          />
        </el-form-item>

        <el-collapse>
          <el-collapse-item title="高级采样设置" name="advanced">
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
          </el-collapse-item>
        </el-collapse>
      </template>
    </el-form>

    <template #footer>
      <el-button @click="close">取消</el-button>
      <el-button type="primary" :loading="submitting" @click="submit">
        {{ isEdit ? '保存' : '创建服务' }}
      </el-button>
    </template>
  </el-dialog>
</template>
