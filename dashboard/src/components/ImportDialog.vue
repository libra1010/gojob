<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { importJobs } from '@/api/job'
import type { ImportJobsResp, JobDTO } from '@/api/types'

const props = defineProps<{ modelValue: boolean; appId: string }>()
const emit = defineEmits<{ 'update:modelValue': [v: boolean]; imported: [] }>()

const raw = ref('')
const submitting = ref(false)
const result = ref<ImportJobsResp | null>(null)
const fileInput = ref<HTMLInputElement>()
/** 已合并进来的文件名，只用于展示"这批是从哪来的" */
const loadedFiles = ref<string[]>([])

/** 文本框里现在有多少条待导入任务；JSON 还没写完时返回 0，不打扰用户 */
const pendingCount = computed(() => {
  const t = raw.value.trim()
  if (!t) return 0
  try {
    const d = JSON.parse(t) as unknown
    return Array.isArray(d) ? d.length : 1
  } catch {
    return 0
  }
})

watch(
  () => props.modelValue,
  (v) => {
    if (v) {
      raw.value = ''
      result.value = null
      loadedFiles.value = []
    }
  },
)

function pickFile() {
  fileInput.value?.click()
}

function clearAll() {
  raw.value = ''
  loadedFiles.value = []
  result.value = null
}

/** 一段文本 → 任务数组；单个对象也接受（单任务导出就是一个对象） */
function parseJobs(text: string, label: string): JobDTO[] | null {
  let data: unknown
  try {
    data = JSON.parse(text)
  } catch {
    ElMessage.error(`${label}：JSON 解析失败，请检查内容`)
    return null
  }
  if (Array.isArray(data)) return data as JobDTO[]
  if (data && typeof data === 'object') return [data as JobDTO]
  ElMessage.error(`${label}：内容需为任务对象或任务对象数组`)
  return null
}

/**
 * 支持一次选多个文件：逐个解析后合并成一个大数组回写到文本框，
 * 于是"要导什么"永远以文本框里的内容为唯一依据（所见即所导）。
 */
async function onFile(e: Event) {
  const input = e.target as HTMLInputElement
  const picked = Array.from(input.files ?? [])
  input.value = ''
  if (!picked.length) return

  const acc: JobDTO[] = []
  if (raw.value.trim()) {
    const prev = parseJobs(raw.value, '文本框里的已有内容')
    if (!prev) return
    acc.push(...prev)
  }
  const names: string[] = []
  for (const f of picked) {
    let jobs: JobDTO[] | null = null
    try {
      jobs = parseJobs(await f.text(), `文件 ${f.name}`)
    } catch {
      ElMessage.error(`读取文件 ${f.name} 失败`)
    }
    if (!jobs) continue
    acc.push(...jobs)
    names.push(`${f.name}（${jobs.length} 条）`)
  }
  if (!names.length) return
  loadedFiles.value = [...loadedFiles.value, ...names]
  raw.value = JSON.stringify(acc, null, 2)
  ElMessage.success(`已合并 ${names.length} 个文件，共 ${acc.length} 条任务待导入`)
}

async function onSubmit() {
  const text = raw.value.trim()
  if (!text) {
    ElMessage.warning('请粘贴 JSON 数组或选择 .json 文件（可多选）')
    return
  }
  const jobs = parseJobs(text, '导入内容')
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
    title="导入任务"
    width="680px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-alert type="info" :closable="false" class="tip">
      支持通用任务包格式（JobDTO 数组，camelCase）：本页面「导出」得到的 .json 可以直接导入，
      <b>PowerJob 控制台导出的任务包同样兼容</b>，一次可选多个文件（自动合并、逐条校验）。
      支持 CRON / 固定频率 / 固定延迟 / 每日固定间隔 + 单机/广播/Map/MapReduce 执行类型；
      旧的 SHELL 类型任务会被自动改写成 BUILT_IN + 官方脚本处理器（脚本正文搬到任务参数），
      PYTHON、SHARDING、WORKFLOW 与 EXTERNAL（JAR 动态容器）会被明确拒绝并给出原因。
    </el-alert>
    <div class="tools">
      <el-button size="small" @click="pickFile">选择 .json 文件（可多选）</el-button>
      <input
        ref="fileInput"
        type="file"
        multiple
        accept=".json,application/json"
        class="hidden-file"
        @change="onFile"
      />
      <el-tag v-if="pendingCount" size="small" type="info">待导入 {{ pendingCount }} 条</el-tag>
      <el-button size="small" text @click="clearAll">清 空</el-button>
    </div>
    <div v-if="loadedFiles.length" class="files">
      已合并文件：<span v-for="n in loadedFiles" :key="n" class="file">{{ n }}</span>
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
.files {
  font-size: 12px;
  color: #909399;
  margin-bottom: 8px;
  line-height: 1.8;
}
.file {
  background: #f5f7fa;
  border-radius: 3px;
  padding: 1px 6px;
  margin-right: 6px;
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
