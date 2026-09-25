<script setup lang="ts">
import { ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { importJobs } from '@/api/job'
import type { ImportJobsResp, PowerJobJob } from '@/api/types'

const props = defineProps<{ modelValue: boolean; appId: string }>()
const emit = defineEmits<{ 'update:modelValue': [v: boolean]; imported: [] }>()

const raw = ref('')
const submitting = ref(false)
const result = ref<ImportJobsResp | null>(null)
const fileInput = ref<HTMLInputElement>()

watch(
  () => props.modelValue,
  (v) => {
    if (v) {
      raw.value = ''
      result.value = null
    }
  },
)

function pickFile() {
  fileInput.value?.click()
}

async function onFile(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  try {
    raw.value = await file.text()
    ElMessage.success(`已读取文件 ${file.name}`)
  } catch {
    ElMessage.error('读取文件失败')
  } finally {
    input.value = ''
  }
}

function parseJobs(): PowerJobJob[] | null {
  const text = raw.value.trim()
  if (!text) {
    ElMessage.warning('请粘贴 JSON 数组或选择 .json 文件')
    return null
  }
  let data: unknown
  try {
    data = JSON.parse(text)
  } catch {
    ElMessage.error('JSON 解析失败，请检查内容')
    return null
  }
  if (Array.isArray(data)) return data as PowerJobJob[]
  // PowerJob 的 /job/export 单任务导出是一个对象而非数组，这里一并接受
  if (data && typeof data === 'object') return [data as PowerJobJob]
  ElMessage.error('导入内容需为 PowerJob 导出格式的任务 JSON（对象或对象数组）')
  return null
}

async function onSubmit() {
  const jobs = parseJobs()
  if (!jobs) return
  submitting.value = true
  try {
    result.value = await importJobs(props.appId, jobs)
    if (result.value.failed === 0) {
      ElMessage.success(`导入完成：${result.value.succeeded} 条全部成功`)
    } else {
      ElMessage.warning(`导入完成：成功 ${result.value.succeeded} 条，失败 ${result.value.failed} 条`)
    }
    emit('imported')
  } catch {
    /* 拦截器已提示 */
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <el-dialog
    :model-value="modelValue"
    title="从 PowerJob 导入任务"
    width="680px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-alert type="info" :closable="false" class="tip">
      粘贴 PowerJob 控制台导出的 JobDTO JSON 数组（camelCase），或选择 .json 文件。
      一期支持 CRON + STANDALONE/SHARDING + BUILT_IN/EXTERNAL 处理器，其余逐条返回失败原因。
      旧的 SHELL 类型任务会被自动改写成 BUILT_IN + 官方脚本处理器（脚本正文搬到任务参数），
      PYTHON 与 EXTERNAL（JAR 动态容器）会被明确拒绝。
    </el-alert>
    <div class="tools">
      <el-button size="small" @click="pickFile">选择 .json 文件</el-button>
      <input ref="fileInput" type="file" accept=".json,application/json" class="hidden-file" @change="onFile" />
      <el-button size="small" text @click="raw = ''">清 空</el-button>
    </div>
    <el-input
      v-model="raw"
      type="textarea"
      :rows="10"
      placeholder='[
  {
    "jobName": "历史数据归档",
    "timeExpressionType": "CRON",
    "timeExpression": "0 0/5 * * * ?",
    "executeType": "STANDALONE",
    "processorType": "BUILT_IN",
    "processorInfo": "com.example.archive.ArchiveProcessor",
    "jobParams": "{\"days\":7}",
    "maxInstanceNum": 1,
    "concurrency": 10,
    "taskRetryNum": 3,
    "enable": true
  }
]'
    />

    <template v-if="result">
      <el-descriptions :column="3" border size="small" class="summary">
        <el-descriptions-item label="总数">{{ result.total }}</el-descriptions-item>
        <el-descriptions-item label="成功">
          <span class="ok">{{ result.succeeded }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="失败">
          <span class="bad">{{ result.failed }}</span>
        </el-descriptions-item>
      </el-descriptions>
      <el-table :data="result.items" size="small" max-height="240" stripe>
        <el-table-column prop="job_name" label="任务名" min-width="150" show-overflow-tooltip />
        <el-table-column label="结果" width="90">
          <template #default="{ row }">
            <el-tag :type="row.success ? 'success' : 'danger'" size="small">
              {{ row.success ? '成功' : '失败' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="任务 ID / 原因" min-width="240">
          <template #default="{ row }">
            <span v-if="row.success" class="ok">#{{ row.job_id }}</span>
            <span v-else class="bad">{{ row.reason }}</span>
          </template>
        </el-table-column>
      </el-table>
    </template>

    <template #footer>
      <el-button @click="emit('update:modelValue', false)">关 闭</el-button>
      <el-button type="primary" :loading="submitting" @click="onSubmit">导 入</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.tip {
  margin-bottom: 10px;
}
.tools {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.hidden-file {
  display: none;
}
.summary {
  margin: 14px 0 10px;
}
.ok {
  color: #67c23a;
}
.bad {
  color: #f56c6c;
}
</style>
