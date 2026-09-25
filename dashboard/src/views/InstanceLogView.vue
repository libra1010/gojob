<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { getInstance, listInstanceTasks, listTaskLogs } from '@/api/instance'
import type { InstanceTask, JobInstance, TaskLog } from '@/api/types'
import { isFinalStatus } from '@/api/types'
import { formatMillis, formatTime, statusTagType } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const instanceId = Number(route.params.instanceId || 0)

/** 未进入终态时每 2 秒增量拉取一次 */
const POLL_MS = 2_000
const LOG_LIMIT = 500

const instance = ref<JobInstance | null>(null)
const tasks = ref<InstanceTask[]>([])
const activeTaskId = ref<number | null>(null)
const logs = ref<TaskLog[]>([])
/** 已见最大 seq，作为 after_seq 游标增量拉取 */
const cursor = ref(0)
const loadingLogs = ref(false)
const lastUpdated = ref('')

// ---------- 日志查看器：贴底自动滚动，用户上滑即暂停 ----------
const viewer = ref<HTMLElement>()
const sticky = ref(true)

function onViewerScroll() {
  const el = viewer.value
  if (!el) return
  sticky.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

async function scrollToEnd() {
  if (!sticky.value) return
  await nextTick()
  const el = viewer.value
  if (el) el.scrollTop = el.scrollHeight
}

/** 用户上滑后手动回到最新 */
async function jumpToLatest() {
  sticky.value = true
  await scrollToEnd()
}

const activeTask = computed(() => tasks.value.find((t) => t.id === activeTaskId.value) ?? null)

/** 实例与当前任务都进入终态后停止轮询 */
const shouldPoll = computed(() => {
  if (!instance.value) return true
  if (!isFinalStatus(instance.value.status)) return true
  const t = activeTask.value
  return t ? !isFinalStatus(t.status) : false
})
const pollState = computed(() => (shouldPoll.value ? '执行中，每 2 秒自动刷新' : '已结束，已停止自动刷新'))

async function loadInstance() {
  instance.value = await getInstance(instanceId)
}

async function loadTasks() {
  const rows = await listInstanceTasks(instanceId)
  tasks.value = rows
  if (activeTaskId.value === null && rows.length) {
    await selectTask(rows[0].id)
    return
  }
  // 任务列表刷新后当前任务已不存在（如实例重试重建任务）：回到首个任务
  if (activeTaskId.value !== null && !rows.some((t) => t.id === activeTaskId.value)) {
    await selectTask(rows.length ? rows[0].id : null)
  }
}

/** 切换任务：清空日志与游标后全量重拉 */
async function selectTask(taskId: number | null) {
  activeTaskId.value = taskId
  logs.value = []
  cursor.value = 0
  sticky.value = true
  if (taskId === null) return
  await pullLogs()
}

async function pullLogs() {
  const taskId = activeTaskId.value
  if (taskId === null) return
  loadingLogs.value = true
  try {
    const rows = await listTaskLogs(taskId, cursor.value, LOG_LIMIT)
    // 拉取期间切了任务：丢弃这批数据
    if (activeTaskId.value !== taskId) return
    if (rows.length) {
      let max = cursor.value
      for (const r of rows) if (r.seq > max) max = r.seq
      logs.value.push(...rows)
      cursor.value = max
    }
    lastUpdated.value = now()
    await scrollToEnd()
  } finally {
    loadingLogs.value = false
  }
}

function now() {
  return formatTime(new Date().toISOString())
}

let ticking = false
async function tick(force = false) {
  if (ticking) return
  if (!force && !shouldPoll.value) return
  ticking = true
  try {
    await loadInstance()
    await loadTasks()
    if (activeTaskId.value !== null) await pullLogs()
  } catch {
    /* 拦截器已提示 */
  } finally {
    ticking = false
  }
}

let timer: number | undefined
onMounted(() => {
  if (!instanceId) {
    ElMessage.error('实例 ID 非法')
    return
  }
  tick(true)
  timer = window.setInterval(() => tick(), POLL_MS)
})
onBeforeUnmount(() => {
  if (timer !== undefined) window.clearInterval(timer)
})

function levelClass(level: string) {
  switch (level) {
    case 'DEBUG':
      return 'lv-debug'
    case 'WARN':
      return 'lv-warn'
    case 'ERROR':
      return 'lv-error'
    default:
      return 'lv-info' // INFO 及未知级别按默认色
  }
}

function taskLabel(t: InstanceTask) {
  return `任务 #${t.id} · ${t.processor}`
}
</script>

<template>
  <el-card shadow="never" class="page">
    <div class="toolbar">
      <div class="left">
        <el-button link @click="router.back()">‹ 返回实例</el-button>
        <h3 class="page-title">实例任务日志 · #{{ instanceId }}</h3>
        <el-tag v-if="instance" :type="statusTagType(instance.status)" size="small">
          {{ instance.status }}
        </el-tag>
        <span v-if="instance" class="meta">
          {{ instance.job_name }}（{{ instance.app_id }}） · 来源
          {{ instance.source || 'CRON' }} · {{ formatTime(instance.create_time) }}
        </span>
      </div>
      <div class="right">
        <span class="updated">{{ pollState }} · 上次 {{ lastUpdated || '-' }}</span>
        <el-button size="small" :loading="loadingLogs" @click="tick(true)">手动刷新</el-button>
      </div>
    </div>

    <el-empty v-if="!tasks.length && !instance" description="未查询到该实例的任务" />
    <div v-else class="body">
      <!-- 左侧：任务列表（单选） -->
      <div class="side">
        <div class="side-head">任务列表（{{ tasks.length }}）</div>
        <div class="side-scroll">
          <el-menu
            :default-active="String(activeTaskId ?? '')"
            class="task-menu"
            @select="(index: string) => selectTask(Number(index))"
          >
            <el-menu-item
              v-for="t in tasks"
              :key="t.id"
              :index="String(t.id)"
              class="task-item"
            >
              <el-tag :type="statusTagType(t.status)" size="small" class="task-tag">
                {{ t.status }}
              </el-tag>
              <div class="task-text">
                <div class="task-name" :title="taskLabel(t)">{{ taskLabel(t) }}</div>
                <div class="task-sub">
                  重试 {{ t.attempts }}/{{ t.max_attempts }} · {{ formatTime(t.dispatch_time) }}
                </div>
              </div>
            </el-menu-item>
          </el-menu>
          <el-empty
            v-if="!tasks.length"
            description="该实例暂无任务记录，可能仍在等待调度"
            :image-size="60"
          />
        </div>
      </div>

      <!-- 右侧：日志查看器 -->
      <div class="main">
        <div v-if="activeTask" class="log-head">
          <div class="log-head-main">
            <el-tag :type="statusTagType(activeTask.status)" size="small">
              {{ activeTask.status }}
            </el-tag>
            <span class="log-title">任务 #{{ activeTask.id }}</span>
            <span class="meta">处理器 {{ activeTask.processor || '-' }}</span>
            <span class="meta">
              重试 {{ activeTask.attempts }}/{{ activeTask.max_attempts }}
            </span>
            <el-tag size="small" effect="plain">日志级别 {{ activeTask.log_level || 'INFO' }}</el-tag>
          </div>
          <div class="log-head-sub">
            <span class="meta">Worker {{ activeTask.worker_id || '-' }}</span>
            <span class="meta">已加载 {{ logs.length }} 行</span>
          </div>
          <div v-if="activeTask.result" class="result">
            <span class="meta">执行结果</span>
            <code>{{ activeTask.result }}</code>
          </div>
        </div>
        <el-alert v-else type="info" :closable="false" title="请选择左侧任务查看日志" />

        <div
          ref="viewer"
          class="viewer"
          v-loading="loadingLogs"
          @scroll="onViewerScroll"
        >
          <div v-for="line in logs" :key="line.id" class="line">
            <span class="ts">{{ formatMillis(line.log_time) }}</span>
            <span class="seq">#{{ line.seq }}</span>
            <span class="lv" :class="levelClass(line.level)">{{ line.level }}</span>
            <span class="msg" :class="levelClass(line.level)">{{ line.content }}</span>
          </div>
          <div v-if="!logs.length && !loadingLogs && activeTask" class="empty">
            该任务暂无上报日志
          </div>
        </div>
        <div class="stick-bar">
          <span class="meta">{{ sticky ? '自动跟随最新日志' : '已暂停自动滚动（向上滚动查看历史）' }}</span>
          <el-button v-if="!sticky && logs.length" link size="small" @click="jumpToLatest">
            回到最新
          </el-button>
        </div>
      </div>
    </div>
  </el-card>
</template>

<style scoped>
.page {
  min-height: calc(100vh - 120px);
}
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.left {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.right {
  display: flex;
  align-items: center;
  gap: 12px;
}
.page-title {
  margin: 0;
}
.meta {
  font-size: 12px;
  color: #909399;
}
.updated {
  font-size: 12px;
  color: #909399;
}
.body {
  display: flex;
  gap: 16px;
  align-items: stretch;
  height: calc(100vh - 240px);
  min-height: 420px;
}
.side {
  width: 300px;
  flex-shrink: 0;
  border: 1px solid #ebeef5;
  border-radius: 4px;
  display: flex;
  flex-direction: column;
}
.side-head {
  padding: 10px 12px;
  font-size: 13px;
  font-weight: 600;
  color: #303133;
  border-bottom: 1px solid #ebeef5;
  background: #fafafa;
}
.side-scroll {
  flex: 1;
  overflow-y: auto;
}
.task-menu {
  border-right: none;
}
.task-item {
  height: auto;
  padding: 10px 12px;
  line-height: 1.4;
  white-space: normal;
}
.task-tag {
  flex-shrink: 0;
  margin-right: 8px;
}
.task-text {
  overflow: hidden;
}
.task-name {
  font-size: 13px;
  color: #303133;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.task-sub {
  font-size: 11px;
  color: #909399;
}
.main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  border: 1px solid #ebeef5;
  border-radius: 4px;
}
.log-head {
  padding: 10px 12px;
  border-bottom: 1px solid #ebeef5;
  background: #fafafa;
}
.log-head-main {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.log-title {
  font-size: 13px;
  font-weight: 600;
  color: #303133;
}
.log-head-sub {
  margin-top: 6px;
  display: flex;
  gap: 16px;
  flex-wrap: wrap;
}
.result {
  margin-top: 6px;
  display: flex;
  align-items: baseline;
  gap: 8px;
}
.result code {
  font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
  font-size: 12px;
  background: #f5f7fa;
  padding: 1px 6px;
  border-radius: 3px;
  word-break: break-all;
}
.viewer {
  flex: 1;
  overflow: auto;
  padding: 10px 12px;
  background: #1e1e1e;
  font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
  font-size: 12.5px;
  line-height: 1.7;
}
.line {
  display: flex;
  gap: 10px;
  align-items: baseline;
  white-space: pre-wrap;
  word-break: break-all;
}
.ts {
  color: #7f8c98;
  flex-shrink: 0;
}
.seq {
  color: #5a6570;
  flex-shrink: 0;
}
.lv {
  flex-shrink: 0;
  width: 52px;
}
.msg {
  color: #d4d4d4;
}
.lv-debug,
.msg.lv-debug {
  color: #808080;
}
.lv-info,
.msg.lv-info {
  color: #d4d4d4;
}
.lv-warn,
.msg.lv-warn {
  color: #e6a23c;
}
.lv-error,
.msg.lv-error {
  color: #f56c6c;
}
.empty {
  color: #7f8c98;
  font-size: 12px;
}
.stick-bar {
  border-top: 1px solid #ebeef5;
  padding: 2px 8px;
  background: #fafafa;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
</style>
