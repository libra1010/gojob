<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { getStatsOverview } from '@/api/stats'
import type { AppStat, StatsOverview } from '@/api/types'
import { formatTime, sdkTagType, statusLabel, statusPillClass } from '@/utils/format'

const router = useRouter()
const stats = ref<StatsOverview | null>(null)
const loading = ref(false)
const lastUpdated = ref('')

/** 页面可见时每 5 秒轮询总览 */
const REFRESH_MS = 5_000
let timer: number | undefined

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    stats.value = await getStatsOverview()
    lastUpdated.value = formatTime(new Date().toISOString())
  } catch {
    /* 拦截器已提示 */
  } finally {
    if (!silent) loading.value = false
  }
}

function startPolling() {
  stopPolling()
  timer = window.setInterval(() => load(true), REFRESH_MS)
}

function stopPolling() {
  if (timer !== undefined) {
    window.clearInterval(timer)
    timer = undefined
  }
}

// 切到后台停轮询，回到前台立刻补一次再排期
function onVisibility() {
  if (document.hidden) {
    stopPolling()
  } else {
    load(true)
    startPolling()
  }
}

onMounted(() => {
  load()
  startPolling()
  document.addEventListener('visibilitychange', onVisibility)
})
onBeforeUnmount(() => {
  stopPolling()
  document.removeEventListener('visibilitychange', onVisibility)
})

const workers = computed(() => stats.value?.workers ?? [])
const apps = computed(() => stats.value?.apps ?? [])
/** 常用项目：任务数多的排前面，最多展示 6 个 */
const commonApps = computed(() =>
  [...apps.value]
    .sort((a, b) => b.job_count - a.job_count)
    .slice(0, 6),
)
const hasMoreApps = computed(() => apps.value.length > 6)
const recentInstances = computed(() => (stats.value?.recent_instances ?? []).slice(0, 10))
const successRate = computed(() =>
  stats.value ? `${stats.value.success_rate.toFixed(1)}%` : '-',
)

function openJobs(app: AppStat) {
  router.push(`/apps/${app.app_id}/jobs`)
}
function openLogs(instanceId: number) {
  router.push(`/instances/${instanceId}/logs`)
}
</script>

<template>
  <div class="overview">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <h3 class="gj-page-title">统计总览</h3>
        <div class="gj-page-sub">
          <span>每 5 秒自动刷新 · 上次 {{ lastUpdated || '-' }}</span>
          <span class="meta-sep" />
          <span>
            对外地址
            <code class="gj-mono addr">{{ stats?.server_address || '-' }}</code>
          </span>
          <el-tag size="small" effect="plain" type="info">{{ stats?.db_driver || '-' }}</el-tag>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-button size="small" :loading="loading" @click="load()">刷 新</el-button>
      </div>
    </div>

    <!-- 运行态势：先回答"今天跑得怎么样"，再给证据表 -->
    <el-card shadow="never" class="posture">
      <div class="posture-main">
        <div class="rate">
          <div class="rate-line">
            <span class="rate-label">今日成功率</span>
            <span class="rate-value gj-num">{{ successRate }}</span>
          </div>
          <div class="rate-bar">
            <span :style="{ width: stats ? successRate : '0%' }" />
          </div>
          <div class="rate-foot">
            今日执行 <b class="gj-num">{{ stats ? stats.today_total : '-' }}</b> 次
          </div>
        </div>

        <div class="posture-stats">
          <div class="pstat">
            <span class="pstat-label"><i class="dot run" />进行中</span>
            <span class="pstat-value gj-num">{{ stats ? stats.today_running : '-' }}</span>
            <span class="pstat-foot">排队与执行中</span>
          </div>
          <div class="pstat">
            <span class="pstat-label"><i class="dot ok" />成功</span>
            <span class="pstat-value gj-num">{{ stats ? stats.today_succeed : '-' }}</span>
            <span class="pstat-foot">今日已完成</span>
          </div>
          <div class="pstat">
            <span class="pstat-label"><i class="dot bad" />失败</span>
            <span
              class="pstat-value gj-num"
              :class="{ bad: (stats?.today_failed ?? 0) > 0 }"
            >{{ stats ? stats.today_failed : '-' }}</span>
            <span class="pstat-foot">需要关注</span>
          </div>
          <div class="pstat">
            <span class="pstat-label"><i class="dot idle" />在线 Worker</span>
            <span class="pstat-value gj-num">{{ stats ? stats.online_workers : '-' }}</span>
            <span class="pstat-foot">心跳注册中</span>
          </div>
        </div>

        <div class="posture-meta">
          <div class="pmeta">
            <span>项目</span>
            <b class="gj-num">{{ stats ? stats.total_apps : '-' }}</b>
          </div>
          <div class="pmeta">
            <span>任务</span>
            <b class="gj-num">{{ stats ? stats.total_jobs : '-' }}</b>
            <em>启用 {{ stats ? stats.enabled_jobs : '-' }}</em>
          </div>
          <div class="pmeta">
            <span>账号</span>
            <b class="gj-num">{{ stats ? stats.total_users : '-' }}</b>
          </div>
        </div>
      </div>
    </el-card>

    <el-row :gutter="16" class="grid">
      <!-- 最近实例是主操作队列 -->
      <el-col :xs="24" :lg="16">
        <el-card shadow="never" class="panel recent-panel">
          <template #header>
            <div class="panel-head">
              <div class="panel-title">
                最近实例
                <span class="panel-sub">最近 {{ recentInstances.length }} 次执行 · 点击实例查看日志</span>
              </div>
              <span class="auto-hint"><i class="live-dot" />自动刷新</span>
            </div>
          </template>
          <el-table :data="recentInstances" v-loading="loading" size="small">
            <el-table-column label="实例" width="88">
              <template #default="{ row }">
                <el-link
                  type="primary"
                  :underline="false"
                  class="gj-mono"
                  @click="openLogs(row.instance_id)"
                >
                  #{{ row.instance_id }}
                </el-link>
              </template>
            </el-table-column>
            <el-table-column prop="job_name" label="任务" min-width="150" show-overflow-tooltip />
            <el-table-column prop="app_id" label="项目" min-width="110" show-overflow-tooltip>
              <template #default="{ row }"><span class="gj-mono">{{ row.app_id }}</span></template>
            </el-table-column>
            <el-table-column label="状态" width="94">
              <template #default="{ row }">
                <span class="gj-pill" :class="statusPillClass(row.status)">
                  {{ statusLabel(row.status) }}
                </span>
              </template>
            </el-table-column>
            <el-table-column label="来源" width="76">
              <template #default="{ row }">
                <span class="source" :class="{ 'source-api': row.source === 'API' }">
                  {{ row.source === 'API' ? '手动' : row.source === 'DEPENDENCY' ? '依赖' : '定时' }}
                </span>
              </template>
            </el-table-column>
            <el-table-column label="创建时间" min-width="150">
              <template #default="{ row }"><span class="time">{{ formatTime(row.create_time) }}</span></template>
            </el-table-column>
            <template #empty>
              <el-empty description="暂无执行记录" :image-size="72" />
            </template>
          </el-table>
        </el-card>
      </el-col>

      <el-col :xs="24" :lg="8">
        <el-card shadow="never" class="panel worker-panel">
          <template #header>
            <div class="panel-head">
              <div class="panel-title">
                在线 Worker
                <span class="panel-sub">{{ workers.length }} 个节点</span>
              </div>
            </div>
          </template>
          <el-empty
            v-if="!workers.length && !loading"
            description="暂无在线节点，等待 Worker 心跳注册"
            :image-size="64"
          />
          <ul v-else class="worker-list">
            <li v-for="w in workers" :key="w.address">
              <span class="worker-dot" />
              <div class="worker-main">
                <div class="worker-addr" :title="w.address">{{ w.address }}</div>
                <div class="worker-meta">
                  <span class="worker-app gj-mono">{{ w.app_id }}</span>
                  <el-tag :type="sdkTagType(w.sdk_type)" size="small" effect="plain">
                    {{ w.sdk_type }}
                  </el-tag>
                </div>
              </div>
              <div class="worker-stats">
                <span class="gj-num">{{ Number(w.memory_used).toFixed(1) }} MB</span>
                <span class="worker-cpu">CPU {{ Number(w.cpu_load).toFixed(2) }}</span>
              </div>
            </li>
          </ul>
        </el-card>
      </el-col>
    </el-row>

    <!-- 常用项目 -->
    <el-card shadow="never" class="panel projects-panel">
      <template #header>
        <div class="panel-head">
          <div class="panel-title">
            常用项目
            <span class="panel-sub">按任务数排序</span>
          </div>
          <el-button v-if="hasMoreApps" link type="primary" @click="router.push('/apps')">
            查看全部 {{ apps.length }} 个 ›
          </el-button>
          <span v-else-if="apps.length" class="panel-sub">{{ apps.length }} 个项目</span>
        </div>
      </template>
      <el-empty
        v-if="!commonApps.length && !loading"
        description="暂无项目，请前往项目管理新建"
        :image-size="72"
      />
      <div v-else class="app-grid">
        <button
          v-for="app in commonApps"
          :key="app.app_id"
          type="button"
          class="app-tile"
          @click="openJobs(app)"
        >
          <div class="app-tile-head">
            <span class="app-name" :title="app.app_name || app.app_id">
              {{ app.app_name || app.app_id }}
            </span>
            <span class="app-jobs gj-num">{{ app.job_count }} 任务</span>
          </div>
          <div class="app-id gj-mono">{{ app.app_id }}</div>
          <div class="app-tile-foot">
            <span
              class="gj-pill"
              :class="app.worker_count > 0 ? 'gj-pill--ok' : 'gj-pill--idle'"
            >{{ app.worker_count }} 节点</span>
            <span class="app-time">{{ formatTime(app.create_time) }}</span>
          </div>
        </button>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.meta-sep {
  width: 1px;
  height: 11px;
  background: var(--gj-border);
}
.addr {
  color: var(--gj-text-regular);
  font-size: 12px;
}

/* ---------- 运行态势 ---------- */
.posture {
  margin-bottom: 16px;
}
.posture :deep(.el-card__body) {
  padding: 0;
}
.posture-main {
  display: flex;
  align-items: stretch;
}
.rate {
  width: 280px;
  flex-shrink: 0;
  padding: 18px 20px;
  background: var(--gj-surface-subtle);
  border-right: 1px solid var(--gj-border-soft);
}
.rate-line {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
}
.rate-label {
  font-size: 12px;
  color: var(--gj-text-muted);
}
.rate-value {
  font-size: 30px;
  font-weight: 700;
  line-height: 1;
  letter-spacing: -0.02em;
  color: var(--gj-primary);
}
.rate-bar {
  height: 6px;
  margin-top: 12px;
  border-radius: 999px;
  background: #e2e9f2;
  overflow: hidden;
}
.rate-bar span {
  display: block;
  height: 100%;
  border-radius: inherit;
  background: var(--gj-primary);
  transition: width 0.3s ease;
}
.rate-foot {
  margin-top: 8px;
  font-size: 12px;
  color: var(--gj-text-muted);
}
.rate-foot b {
  color: var(--gj-text);
  font-size: 13px;
}

.posture-stats {
  flex: 1;
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
}
.pstat {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 16px 20px;
  border-right: 1px solid var(--gj-border-soft);
}
.pstat-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--gj-text-muted);
}
.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  flex-shrink: 0;
}
.dot.ok {
  background: var(--gj-success);
}
.dot.bad {
  background: var(--gj-danger);
}
.dot.run {
  background: var(--gj-primary);
}
.dot.idle {
  background: var(--gj-text-faint);
}
.pstat-value {
  font-size: 24px;
  font-weight: 650;
  line-height: 1.15;
  letter-spacing: -0.01em;
  color: var(--gj-text);
}
.pstat-value.bad {
  color: var(--gj-danger);
}
.pstat-foot {
  font-size: 11px;
  color: var(--gj-text-faint);
}

.posture-meta {
  display: flex;
  align-items: center;
  gap: 22px;
  padding: 16px 20px;
  background: var(--gj-surface-subtle);
  border-left: 1px solid var(--gj-border-soft);
}
.pmeta {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.pmeta span {
  font-size: 11px;
  color: var(--gj-text-muted);
}
.pmeta b {
  font-size: 16px;
  font-weight: 650;
  color: var(--gj-text);
}
.pmeta em {
  font-style: normal;
  font-size: 10px;
  color: var(--gj-text-faint);
}

/* ---------- 面板 ---------- */
.panel {
  margin-bottom: 16px;
}
.panel :deep(.el-card__header) {
  padding: 11px 16px;
}
.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.panel-title {
  display: flex;
  align-items: baseline;
  gap: 10px;
  font-size: 14px;
  font-weight: 600;
  color: var(--gj-text);
}
.panel-sub {
  font-size: 12px;
  font-weight: 400;
  color: var(--gj-text-muted);
}
.auto-hint {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--gj-text-faint);
}
.live-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--gj-success);
  box-shadow: 0 0 0 3px rgba(6, 118, 71, 0.12);
}
.recent-panel :deep(.el-card__body) {
  padding: 4px 16px 12px;
}
.source {
  font-size: 12px;
  color: var(--gj-text-muted);
}
.source-api {
  color: var(--gj-warning);
}
.time {
  font-size: 12px;
  color: var(--gj-text-muted);
  font-variant-numeric: tabular-nums;
}

/* ---------- Worker 列表 ---------- */
.worker-panel :deep(.el-card__body) {
  padding: 14px 16px 16px;
}
.worker-list {
  margin: 0;
  padding: 0;
  list-style: none;
}
.worker-list li {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 0;
  border-bottom: 1px dashed var(--gj-border-soft);
}
.worker-list li:first-child {
  padding-top: 0;
}
.worker-list li:last-child {
  padding-bottom: 0;
  border-bottom: none;
}
.worker-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--gj-success);
  box-shadow: 0 0 0 3px rgba(6, 118, 71, 0.12);
  flex-shrink: 0;
}
.worker-main {
  flex: 1;
  min-width: 0;
}
.worker-addr {
  font-family: var(--gj-mono);
  font-size: 12px;
  color: var(--gj-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.worker-meta {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 3px;
}
.worker-app {
  font-size: 11px;
  color: var(--gj-text-muted);
}
.worker-stats {
  display: flex;
  flex-direction: column;
  flex-shrink: 0;
  text-align: right;
  font-size: 11px;
  color: var(--gj-text-muted);
}
.worker-cpu {
  color: var(--gj-text-faint);
}

/* ---------- 常用项目 ---------- */
.projects-panel :deep(.el-card__body) {
  padding: 14px 16px 16px;
}
.app-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  gap: 10px;
}
.app-tile {
  padding: 12px 14px;
  border: 1px solid var(--gj-border-soft);
  border-radius: var(--gj-radius-sm);
  background: var(--gj-surface);
  text-align: left;
  font: inherit;
  cursor: pointer;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.app-tile:hover {
  border-color: #b9cdea;
  box-shadow: 0 6px 16px -14px rgba(16, 24, 40, 0.45);
}
.app-tile:focus-visible {
  outline: 2px solid var(--gj-primary);
  outline-offset: 2px;
}
.app-tile-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.app-name {
  font-size: 13.5px;
  font-weight: 600;
  color: var(--gj-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.app-jobs {
  flex-shrink: 0;
  font-size: 11px;
  color: var(--gj-text-muted);
}
.app-id {
  margin-top: 2px;
  font-size: 11px;
  color: var(--gj-text-faint);
}
.app-tile-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: 10px;
}
.app-time {
  font-size: 11px;
  color: var(--gj-text-faint);
}

/* ---------- 响应式 ---------- */
@media (max-width: 1280px) {
  .posture-main {
    flex-wrap: wrap;
  }
  .rate {
    width: 100%;
    border-right: none;
    border-bottom: 1px solid var(--gj-border-soft);
  }
  .posture-stats {
    order: 3;
    width: 100%;
    border-top: 1px solid var(--gj-border-soft);
  }
  .posture-meta {
    flex: 1;
    justify-content: flex-end;
    border-left: none;
    background: transparent;
  }
}
@media (max-width: 720px) {
  .posture-stats {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .pstat:nth-child(2n) {
    border-right: none;
  }
  .pstat:nth-child(-n + 2) {
    border-bottom: 1px solid var(--gj-border-soft);
  }
  .posture-meta {
    width: 100%;
    justify-content: flex-start;
    border-top: 1px solid var(--gj-border-soft);
  }
}
@media (prefers-reduced-motion: reduce) {
  .rate-bar span,
  .app-tile {
    transition: none;
  }
}
</style>
