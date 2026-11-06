<script setup lang="ts">
// 实例详情弹窗：信息构成与兼容协议的 InstanceDetail.vue 1:1 对齐——
// Task 统计、子任务表格（列名与顺序一致）、"select * from task_info where ___ limit 10"
// 那个查询框，以及任务结果。
//
// 差别只有一处，而且是能力增强：兼容协议的明细是 server 现场反向 ask TaskTracker 所在
// worker 查它的本地表，worker 一掉线或实例一结束就查不到了；gojob 的任务树在库里，
// 所以任何时候打开这个弹窗都能看到当时的每一片。
import { ref, watch } from 'vue'
import { instanceDetail } from '@/api/instance'
import type { InstanceDetail } from '@/api/types'
import { formatTime, sourceLabel, statusLabel, statusPillClass } from '@/utils/format'

const props = defineProps<{ modelValue: boolean; instanceId: number }>()
const emit = defineEmits<{ 'update:modelValue': [boolean] }>()

// 默认查询就是"只看已失败的"（沿用兼容协议的口径与文案）
const DEFAULT_QUERY = 'status in (5, 6) order by last_modified_time desc'
const query = ref(DEFAULT_QUERY)
const detail = ref<InstanceDetail | null>(null)
const loading = ref(false)
const error = ref('')

async function load() {
  if (!props.instanceId) return
  loading.value = true
  error.value = ''
  try {
    detail.value = await instanceDetail(props.instanceId, query.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : '加载失败'
  } finally {
    loading.value = false
  }
}

watch(
  () => [props.modelValue, props.instanceId] as const,
  ([open]) => {
    if (open) {
      query.value = DEFAULT_QUERY
      load()
    }
  },
)

const statsText = () => {
  const s = detail.value?.task_stats
  if (!s) return ''
  // 统计按一段 JSON 原文展示（同兼容协议），排障时可以直接复制
  return JSON.stringify(s)
}

const isInstanceFinal = () => {
  const st = detail.value?.instance.status || ''
  return ['SUCCEED', 'FAILED', 'STOPPED'].includes(st)
}

function executeLabel(type?: string) {
  switch (type) {
    case 'BROADCAST':
      return '广播执行'
    case 'MAP':
      return 'Map'
    case 'MAP_REDUCE':
      return 'MapReduce'
    default:
      return '单机执行'
  }
}
</script>

<template>
  <el-dialog
    :model-value="modelValue"
    @update:model-value="emit('update:modelValue', $event)"
    :title="`实例详情 · #${instanceId}`"
    width="80%"
    :close-on-click-modal="false"
  >
    <div v-loading="loading">
      <el-descriptions v-if="detail" :column="3" border size="small">
        <el-descriptions-item label="任务名称">{{ detail.instance.job_name }}</el-descriptions-item>
        <el-descriptions-item label="状态">
          <span class="gj-pill" :class="statusPillClass(detail.instance.status)">
            {{ statusLabel(detail.instance.status) }}
          </span>
        </el-descriptions-item>
        <el-descriptions-item label="执行类型">
          {{ executeLabel(detail.instance.execute_type) }}
        </el-descriptions-item>
        <el-descriptions-item label="处理器">{{ detail.instance.processor }}</el-descriptions-item>
        <el-descriptions-item label="触发来源">{{ sourceLabel(detail.instance.source) }}</el-descriptions-item>
        <el-descriptions-item label="重试次数">{{ detail.instance.try_times }}</el-descriptions-item>
        <el-descriptions-item label="创建时间">{{ formatTime(detail.instance.create_time) }}</el-descriptions-item>
        <el-descriptions-item label="最后上报">{{ formatTime(detail.instance.modify_time) }}</el-descriptions-item>
        <el-descriptions-item label="未结束任务">{{ detail.instance.running_times }}</el-descriptions-item>
      </el-descriptions>

      <div class="stats">
        <span class="label">子任务统计</span>
        <code>{{ statsText() }}</code>
        <el-button size="small" :loading="loading" @click="load">重 查</el-button>
      </div>

      <div class="query">
        <span class="prefix">select * from task_info where</span>
        <el-input v-model="query" size="small" placeholder="status in (5, 6) order by last_modified_time desc" />
        <span class="suffix">limit 10</span>
        <el-button size="small" type="primary" :loading="loading" @click="load">查 询</el-button>
      </div>
      <el-alert
        v-if="detail?.query_error"
        type="warning"
        :closable="false"
        show-icon
        :title="detail.query_error"
        class="query-error"
      />
      <div class="hint">
        这一句不会原样进数据库：后端只认 status / task_name / parent_task_id 的等值与 IN
        写法，加可选的 order by；条数固定 10。status 的数字按任务状态序号解释（5=失败、6=成功）。
      </div>

      <el-table :data="detail?.tasks || []" size="small" stripe max-height="320" class="tasks">
        <el-table-column prop="task_id" label="任务 ID" width="110" />
        <el-table-column prop="task_name" label="任务名" min-width="120" show-overflow-tooltip />
        <el-table-column prop="task_content" label="任务内容" min-width="160" show-overflow-tooltip />
        <el-table-column prop="processor_addr" label="执行节点" min-width="140" show-overflow-tooltip />
        <el-table-column prop="failed_cnt" label="失败数" width="70" />
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <span class="gj-pill" :class="statusPillClass(row.status)">
              {{ statusLabel(row.status_str || row.status) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="160">
          <template #default="{ row }">{{ formatTime(row.created_time) }}</template>
        </el-table-column>
        <el-table-column label="最后修改" width="160">
          <template #default="{ row }">{{ formatTime(row.last_modified_time) }}</template>
        </el-table-column>
        <el-table-column prop="result" label="任务结果" min-width="200" show-overflow-tooltip />
        <template #empty>
          <el-empty description="没有命中该查询条件的任务" :image-size="60" />
        </template>
      </el-table>
      <div v-if="detail" class="total">命中 {{ detail.total }} 条，本页显示前 {{ detail.tasks?.length || 0 }} 条</div>

      <div class="result">
        <span class="label">任务结果:</span>
        <pre>{{ detail?.instance.result || (isInstanceFinal() ? '（无）' : '执行中，尚无结果') }}</pre>
      </div>
      <div v-if="error" class="error">{{ error }}</div>
    </div>
    <template #footer>
      <el-button @click="emit('update:modelValue', false)">关 闭</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.stats {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 12px 0;
}
.stats code {
  background: var(--gj-surface-subtle);
  padding: 2px 6px;
  border-radius: 3px;
  font-family: var(--gj-mono);
  font-size: 12px;
}
.query {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 6px;
}
.query :deep(.el-input) {
  max-width: 460px;
}
.prefix,
.suffix {
  font-family: var(--gj-mono);
  font-size: 12px;
  color: var(--gj-text-muted);
  white-space: nowrap;
}
.query-error {
  margin-bottom: 6px;
}
.hint {
  font-size: 12px;
  color: var(--gj-text-muted);
  margin-bottom: 8px;
}
.tasks {
  margin-bottom: 4px;
}
.total {
  font-size: 12px;
  color: var(--gj-text-muted);
  margin-bottom: 10px;
}
.result {
  margin-top: 10px;
}
.result .label {
  font-weight: 600;
}
.result pre {
  margin: 4px 0 0;
  white-space: pre-wrap;
  word-break: break-all;
  background: var(--gj-surface-subtle);
  padding: 8px;
  border-radius: 4px;
  max-height: 120px;
  overflow: auto;
  font-family: var(--gj-mono);
  font-size: 12px;
}
.error {
  color: var(--gj-danger);
  margin-top: 8px;
}
</style>
