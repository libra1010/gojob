<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { listInstances, stopInstance } from '@/api/instance'
import type { JobInstance } from '@/api/types'
import InstanceDetailDialog from '@/components/InstanceDetailDialog.vue'
import { useAuthStore } from '@/stores/auth'
import { formatTime, statusLabel, statusPillClass } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const jobId = Number(route.params.jobId || 0)
const auth = useAuthStore()

const instances = ref<JobInstance[]>([])
const loading = ref(false)
const lastUpdated = ref('')
// 实例详情弹窗（行内"详情"）
const detailOpen = ref(false)
const detailId = ref(0)

function openDetail(row: JobInstance) {
  detailId.value = row.id
  detailOpen.value = true
}

const REFRESH_MS = 10_000
let timer: number | undefined

/** 未进终态的实例才允许停止；STOPPING 表示指令已下达、等 worker 协作退出 */
function stoppable(row: JobInstance) {
  return ['WAITING', 'RUNNING', 'STOPPING'].includes(row.status)
}

async function onStop(row: JobInstance) {
  try {
    await ElMessageBox.confirm(
      `确定停止实例 #${row.id}？未派发的任务当场终止；正在执行的任务会收到停止指令，由处理器协作退出。`,
      '停止确认',
      { type: 'warning', confirmButtonText: '停 止', cancelButtonText: '取 消' },
    )
  } catch {
    return
  }
  try {
    await stopInstance(row.id, row.app_id)
    ElMessage.success('停止指令已下达')
    load(true)
  } catch {
    /* 拦截器已提示（实例已终态等） */
  }
}

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    instances.value = await listInstances(jobId)
    lastUpdated.value = formatTime(new Date().toISOString())
  } catch {
    /* 拦截器已提示 */
  } finally {
    if (!silent) loading.value = false
  }
}

onMounted(() => {
  load()
  timer = window.setInterval(() => load(true), REFRESH_MS)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<template>
  <div class="instances-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <button type="button" class="gj-back" @click="router.back()">‹ 返回任务</button>
        <h3 class="gj-page-title">
          执行实例
          <span class="job-chip">任务 #{{ jobId }}</span>
        </h3>
        <div class="gj-page-sub">
          <span>每 10 秒自动刷新 · 上次 {{ lastUpdated || '-' }}</span>
          <span class="meta-sep" />
          <span>共 {{ instances.length }} 条</span>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-button size="small" :loading="loading" @click="load()">刷 新</el-button>
      </div>
    </div>

    <el-card shadow="never" class="table-card">
      <el-table :data="instances" v-loading="loading" size="small">
        <el-table-column label="实例" width="84">
          <template #default="{ row }"><span class="gj-mono">#{{ row.id }}</span></template>
        </el-table-column>
        <el-table-column prop="job_name" label="任务名" min-width="140" show-overflow-tooltip />
        <el-table-column label="状态" width="96">
          <template #default="{ row }">
            <span class="gj-pill" :class="statusPillClass(row.status)">
              {{ statusLabel(row.status) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="来源" width="108">
          <template #default="{ row }">
            <el-tooltip
              v-if="row.source === 'DEPENDENCY'"
              :content="`上游实例 #${row.upstream_instance_id ?? '-'} 执行成功后自动触发`"
              placement="top"
            >
              <el-tag type="success" size="small" effect="plain">依赖</el-tag>
            </el-tooltip>
            <el-tag
              v-else-if="row.source === 'API'"
              type="warning"
              size="small"
              effect="plain"
            >手动</el-tag>
            <el-tag v-else size="small" effect="plain" type="info">定时</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="processor" label="处理器" min-width="170" show-overflow-tooltip />
        <el-table-column label="重试次数" width="90">
          <template #default="{ row }"><span class="gj-num">{{ row.try_times }}</span></template>
        </el-table-column>
        <el-table-column label="任务进度" width="110">
          <template #default="{ row }">
            <span class="gj-num progress">
              {{ row.total_task_num - row.failed_task_num }} / {{ row.total_task_num }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" min-width="150">
          <template #default="{ row }"><span class="time">{{ formatTime(row.create_time) }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="210" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
              <el-button size="small" @click="openDetail(row)">详 情</el-button>
              <el-button size="small" @click="router.push(`/instances/${row.id}/logs`)">日 志</el-button>
              <el-button
                v-if="stoppable(row) && auth.canWriteApp(row.app_id)"
                size="small"
                type="danger"
                plain
                @click="onStop(row)"
              >停 止</el-button>
            </div>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无执行实例" :image-size="72" />
        </template>
      </el-table>
    </el-card>
  </div>

  <InstanceDetailDialog v-model="detailOpen" :instance-id="detailId" />
</template>

<style scoped>
.job-chip {
  display: inline-block;
  margin-left: 8px;
  padding: 1px 7px;
  border-radius: 4px;
  background: var(--gj-primary-soft);
  color: var(--gj-primary);
  font-size: 11px;
  font-weight: 500;
  vertical-align: 2px;
}
.meta-sep {
  width: 1px;
  height: 11px;
  background: var(--gj-border);
}
.table-card :deep(.el-card__body) {
  padding: 6px 16px 12px;
}
.progress {
  font-size: 12px;
  color: var(--gj-text-regular);
}
.time {
  font-size: 12px;
  color: var(--gj-text-muted);
  font-variant-numeric: tabular-nums;
}
.row-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}
</style>
