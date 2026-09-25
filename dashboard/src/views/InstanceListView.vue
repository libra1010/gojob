<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { listInstances, stopInstance } from '@/api/instance'
import type { JobInstance } from '@/api/types'
import InstanceDetailDialog from '@/components/InstanceDetailDialog.vue'
import { useAuthStore } from '@/stores/auth'
import { formatTime, statusTagType } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const jobId = Number(route.params.jobId || 0)
const auth = useAuthStore()

const instances = ref<JobInstance[]>([])
const loading = ref(false)
const lastUpdated = ref('')
// 实例详情弹窗（PowerJob 的行内"详情"）
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
  <el-card shadow="never">
    <div class="toolbar">
      <div class="left">
        <el-button link @click="router.back()">‹ 返回任务</el-button>
        <h3 class="page-title">执行实例 · 任务 #{{ jobId }}</h3>
      </div>
      <div class="right">
        <span class="updated">每 10 秒自动刷新 · 上次 {{ lastUpdated || '-' }}</span>
        <el-button size="small" :loading="loading" @click="load()">刷 新</el-button>
      </div>
    </div>

    <el-table :data="instances" v-loading="loading" stripe>
      <el-table-column prop="id" label="实例 ID" width="90" />
      <el-table-column prop="job_name" label="任务名" min-width="140" show-overflow-tooltip />
      <el-table-column label="状态" width="100">
        <template #default="{ row }">
          <el-tag :type="statusTagType(row.status)" size="small">{{ row.status }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="source" label="来源" width="120">
        <template #default="{ row }">
          <el-tag v-if="row.source === 'API'" type="warning" size="small" effect="plain">API</el-tag>
          <el-tooltip
            v-else-if="row.source === 'DEPENDENCY'"
            :content="`上游实例 #${row.upstream_instance_id ?? '-'} 执行成功后自动触发`"
            placement="top"
          >
            <el-tag type="success" size="small" effect="plain">DEPENDENCY</el-tag>
          </el-tooltip>
          <el-tag v-else size="small" effect="plain">{{ row.source || 'CRON' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="processor" label="处理器" min-width="170" show-overflow-tooltip />
      <el-table-column label="重试次数" width="90">
        <template #default="{ row }">{{ row.try_times }}</template>
      </el-table-column>
      <el-table-column label="任务进度" width="110">
        <template #default="{ row }">{{ row.total_task_num - row.failed_task_num }} / {{ row.total_task_num }}</template>
      </el-table-column>
      <el-table-column label="创建时间" min-width="160">
        <template #default="{ row }">{{ formatTime(row.create_time) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="230" fixed="right">
        <template #default="{ row }">
          <el-button size="small" @click="openDetail(row)">详 情</el-button>
          <el-button size="small" @click="router.push(`/instances/${row.id}/logs`)">日 志</el-button>
          <el-button
            v-if="stoppable(row) && auth.canWriteApp(row.app_id)"
            size="small"
            type="danger"
            plain
            @click="onStop(row)"
            >停 止</el-button
          >
        </template>
      </el-table-column>
      <template #empty>
        <el-empty description="暂无执行实例" />
      </template>
    </el-table>
  </el-card>

  <InstanceDetailDialog v-model="detailOpen" :instance-id="detailId" />
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 4px;
}
.left {
  display: flex;
  align-items: center;
  gap: 8px;
}
.right {
  display: flex;
  align-items: center;
  gap: 12px;
}
.updated {
  font-size: 12px;
  color: #909399;
}
.page-title {
  margin: 0;
}
</style>
