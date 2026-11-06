<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { getInstance, listInstanceTasks, listTaskLogs } from '@/api/instance'
import type { InstanceTask, JobInstance, TaskLog } from '@/api/types'
import { isFinalStatus } from '@/api/types'
import { formatMillis, formatTime, sourceLabel, statusLabel, statusPillClass } from '@/utils/format'

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
  <div class="log-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <button type="button" class="gj-back" @click="router.back()">‹ 返回实例</button>
        <h3 class="gj-page-title">
          实例日志
          <span class="id-chip gj-mono">#{{ instanceId }}</span>
        </h3>
        <div class="gj-page-sub">
          <span v-if="instance" class="gj-pill" :class="statusPillClass(instance.status)">
            {{ statusLabel(instance.status) }}
          </span>
          <template v-if="instance">
            <span class="head-job">{{ instance.job_name }}</span>
            <span class="meta-sep" />
            <span class="gj-mono">{{ instance.app_id }}</span>
            <span class="meta-sep" />
            <span>来源 {{ sourceLabel(instance.source) }}</span>
            <span class="meta-sep" />
            <span>{{ formatTime(instance.create_time) }}</span>
            <span class="meta-sep" />
          </template>
          <span>{{ pollState }}</span>
        </div>
      </div>
      <div class="gj-page-actions">
        <span class="updated">上次 {{ lastUpdated || '-' }}</span>
        <el-button size="small" :loading="loadingLogs" @click="tick(true)">手动刷新</el-button>
      </div>
    </div>

    <el-empty v-if="!tasks.length && !instance" description="未查询到该实例的任务" />
    <div v-else class="body">
      <!-- 左侧：任务列表（单选） -->
      <aside class="side">
        <div class="side-head">
          <span>任务列表</span>
          <span class="side-count gj-num">{{ tasks.length }}</span>
        </div>
        <div class="side-scroll">
          <button
            v-for="t in tasks"
            :key="t.id"
            type="button"
            class="task-item"
            :class="{ active: t.id === activeTaskId }"
            @click="selectTask(t.id)"
          >
            <span class="gj-pill task-pill" :class="statusPillClass(t.status)">
              {{ statusLabel(t.status) }}
            </span>
            <span class="task-text">
              <span class="task-name" :title="taskLabel(t)">{{ taskLabel(t) }}</span>
              <span class="task-sub">
                重试 {{ t.attempts }}/{{ t.max_attempts }} · {{ formatTime(t.dispatch_time) }}
              </span>
            </span>
          </button>
          <el-empty
            v-if="!tasks.length"
            description="该实例暂无任务记录，可能仍在等待调度"
            :image-size="60"
          />
        </div>
      </aside>

      <!-- 右侧：日志查看器 -->
      <section class="main">
        <div v-if="activeTask" class="log-head">
          <div class="log-head-main">
            <span class="gj-pill" :class="statusPillClass(activeTask.status)">
              {{ statusLabel(activeTask.status) }}
            </span>
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
      </section>
    </div>
  </div>
</template>

<style scoped>
.id-chip {
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
.head-job {
  font-weight: 600;
  color: var(--gj-text-regular);
}
.meta-sep {
  width: 1px;
  height: 11px;
  background: var(--gj-border);
}
.updated {
  font-size: 12px;
  color: var(--gj-text-faint);
  font-variant-numeric: tabular-nums;
}
.meta {
  font-size: 12px;
  color: var(--gj-text-muted);
}
.body {
  display: grid;
  grid-template-columns: 288px minmax(0, 1fr);
  gap: 16px;
  height: calc(100vh - 232px);
  min-height: 460px;
}
.side,
.main {
  display: flex;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
  border: 1px solid var(--gj-border);
  border-radius: var(--gj-radius);
  background: var(--gj-surface);
}
.side-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 11px 14px;
  border-bottom: 1px solid var(--gj-border-soft);
  font-size: 13px;
  font-weight: 600;
  color: var(--gj-text);
}
.side-count {
  font-size: 12px;
  font-weight: 500;
  color: var(--gj-text-muted);
}
.side-scroll {
  flex: 1;
  overflow-y: auto;
}
.task-item {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  width: 100%;
  padding: 11px 14px;
  border: 0;
  border-bottom: 1px solid var(--gj-border-soft);
  border-left: 3px solid transparent;
  background: transparent;
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: background-color 0.12s ease;
}
.task-item:hover {
  background: var(--gj-surface-subtle);
}
.task-item.active {
  border-left-color: var(--gj-primary);
  background: var(--gj-primary-soft);
}
.task-item:focus-visible {
  outline: 2px solid var(--gj-primary);
  outline-offset: -2px;
}
.task-pill {
  flex-shrink: 0;
  margin-top: 1px;
}
.task-text {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}
.task-name {
  font-size: 13px;
  color: var(--gj-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.task-sub {
  font-size: 11px;
  color: var(--gj-text-muted);
  font-variant-numeric: tabular-nums;
}
.main {
  flex: 1;
  min-width: 0;
}
.log-head {
  padding: 12px 14px;
  border-bottom: 1px solid var(--gj-border-soft);
  background: var(--gj-surface-subtle);
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
  color: var(--gj-text);
}
.log-head-sub {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  margin-top: 7px;
}
.result {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-top: 7px;
}
.result code {
  padding: 1px 6px;
  border: 1px solid var(--gj-border-soft);
  border-radius: 4px;
  background: var(--gj-surface);
  font-family: var(--gj-mono);
  font-size: 12px;
  color: var(--gj-text-regular);
  word-break: break-all;
}
.viewer {
  flex: 1;
  overflow: auto;
  padding: 12px 14px;
  background: #0b1524;
  font-family: var(--gj-mono);
  font-size: 12.5px;
  line-height: 1.75;
}
.line {
  display: flex;
  gap: 10px;
  align-items: baseline;
  white-space: pre-wrap;
  word-break: break-all;
}
.ts {
  flex-shrink: 0;
  color: #64748b;
  font-variant-numeric: tabular-nums;
}
.seq {
  flex-shrink: 0;
  color: #475569;
}
.lv {
  flex-shrink: 0;
  width: 44px;
}
.msg {
  color: #cbd5e1;
}
.lv-debug,
.msg.lv-debug {
  color: #94a3b8;
}
.lv-info,
.msg.lv-info {
  color: #cbd5e1;
}
.lv-warn,
.msg.lv-warn {
  color: #fbbf24;
}
.lv-error,
.msg.lv-error {
  color: #f87171;
}
.empty {
  color: #64748b;
  font-size: 12px;
}
.stick-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 3px 14px;
  border-top: 1px solid var(--gj-border-soft);
  background: var(--gj-surface-subtle);
}

@media (max-width: 900px) {
  .body {
    grid-template-columns: minmax(0, 1fr);
    height: auto;
  }
  .side {
    max-height: 240px;
  }
  .main {
    height: calc(100vh - 300px);
    min-height: 420px;
  }
}
</style>
