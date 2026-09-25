<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { getStatsOverview } from '@/api/stats'
import type { AppStat, StatsOverview } from '@/api/types'
import { formatTime, sdkTagType, statusTagType } from '@/utils/format'

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
    <div class="toolbar">
      <h3 class="page-title">统计总览</h3>
      <div class="right">
        <span class="updated">每 5 秒自动刷新 · 上次 {{ lastUpdated || '-' }}</span>
        <el-button size="small" :loading="loading" @click="load()">刷 新</el-button>
      </div>
    </div>

    <!-- 指标卡 -->
    <el-row :gutter="16" class="tiles">
      <el-col :xs="12" :sm="8" :md="8" :lg="4">
        <el-card shadow="hover" class="tile">
          <div class="tile-label">账号数</div>
          <div class="tile-value">{{ stats ? stats.total_users : '-' }}</div>
          <div class="tile-foot">控制台可登录账号</div>
        </el-card>
      </el-col>
      <el-col :xs="12" :sm="8" :md="8" :lg="4">
        <el-card shadow="hover" class="tile">
          <div class="tile-label">在线 Worker 数</div>
          <div class="tile-value ok">{{ stats ? stats.online_workers : '-' }}</div>
          <div class="tile-foot">心跳注册中</div>
        </el-card>
      </el-col>
      <el-col :xs="12" :sm="8" :md="8" :lg="4">
        <el-card shadow="hover" class="tile">
          <div class="tile-label">任务总数</div>
          <div class="tile-value">{{ stats ? stats.total_jobs : '-' }}</div>
          <div class="tile-foot">启用 {{ stats ? stats.enabled_jobs : '-' }} 个</div>
        </el-card>
      </el-col>
      <el-col :xs="12" :sm="8" :md="8" :lg="4">
        <el-card shadow="hover" class="tile" @click="router.push('/apps')">
          <div class="tile-label">项目总数</div>
          <div class="tile-value">{{ stats ? stats.total_apps : '-' }}</div>
          <div class="tile-foot link">点击查看项目列表</div>
        </el-card>
      </el-col>
      <el-col :xs="12" :sm="8" :md="8" :lg="4">
        <el-card shadow="hover" class="tile">
          <div class="tile-label">今日执行</div>
          <div class="tile-value">{{ stats ? stats.today_total : '-' }}</div>
          <div class="tile-foot">
            <span class="ok">成功 {{ stats ? stats.today_succeed : 0 }}</span>
            <span class="bad">失败 {{ stats ? stats.today_failed : 0 }}</span>
            <span>进行中 {{ stats ? stats.today_running : 0 }}</span>
          </div>
          <div class="tile-foot">成功率 {{ successRate }}</div>
        </el-card>
      </el-col>
      <el-col :xs="12" :sm="8" :md="8" :lg="4">
        <el-card shadow="hover" class="tile">
          <div class="tile-label">对外访问地址</div>
          <div class="tile-value addr" :title="stats?.server_address">
            {{ stats?.server_address || '-' }}
          </div>
          <div class="tile-foot">
            数据库类型
            <el-tag size="small" effect="plain" type="info">{{ stats?.db_driver || '-' }}</el-tag>
          </div>
        </el-card>
      </el-col>
    </el-row>

    <!-- 在线 Worker 节点 -->
    <el-card shadow="never" class="section">
      <template #header>
        <div class="section-header">
          <span class="section-title">在线 Worker 节点</span>
          <span class="section-sub">共 {{ workers.length }} 个</span>
        </div>
      </template>
      <el-table :data="workers" v-loading="loading" size="small" stripe>
        <el-table-column prop="address" label="地址" min-width="220" show-overflow-tooltip />
        <el-table-column prop="app_id" label="项目 ID" min-width="140" show-overflow-tooltip />
        <el-table-column label="SDK 类型" width="160">
          <template #default="{ row }">
            <el-tag :type="sdkTagType(row.sdk_type)" size="small">{{ row.sdk_type }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="version" label="版本" min-width="140" show-overflow-tooltip />
        <el-table-column label="内存(MB)" width="110">
          <template #default="{ row }">{{ Number(row.memory_used).toFixed(1) }}</template>
        </el-table-column>
        <el-table-column label="CPU 负载" width="100">
          <template #default="{ row }">{{ Number(row.cpu_load).toFixed(2) }}</template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无在线 Worker 节点，等待 Worker 心跳注册" :image-size="80" />
        </template>
      </el-table>
    </el-card>

    <!-- 常用项目 -->
    <el-card shadow="never" class="section">
      <template #header>
        <div class="section-header">
          <span class="section-title">常用项目</span>
          <el-button v-if="hasMoreApps" link type="primary" @click="router.push('/apps')">
            查看全部 {{ apps.length }} 个项目 ›
          </el-button>
          <span v-else class="section-sub">共 {{ apps.length }} 个项目</span>
        </div>
      </template>
      <el-empty
        v-if="!commonApps.length && !loading"
        description="暂无项目，请前往项目管理新建"
        :image-size="80"
      />
      <el-row v-else :gutter="16">
        <el-col v-for="app in commonApps" :key="app.app_id" :xs="24" :sm="12" :md="8">
          <el-card shadow="hover" class="app-card">
            <div class="app-card-head">
              <span class="app-name" :title="app.app_name || app.app_id">
                {{ app.app_name || app.app_id }}
              </span>
              <el-link type="primary" :underline="false" @click="openJobs(app)">查看任务</el-link>
            </div>
            <div class="app-id">{{ app.app_id }}</div>
            <ul class="app-metrics">
              <li>
                <span class="m-label">任务数</span>
                <span class="m-value">{{ app.job_count }}</span>
              </li>
              <li>
                <span class="m-label">在线节点数</span>
                <span class="m-value">{{ app.worker_count }}</span>
              </li>
              <li>
                <span class="m-label">创建时间</span>
                <span class="m-value">{{ formatTime(app.create_time) }}</span>
              </li>
            </ul>
          </el-card>
        </el-col>
      </el-row>
    </el-card>

    <!-- 最近实例 -->
    <el-card shadow="never" class="section">
      <template #header>
        <div class="section-header">
          <span class="section-title">最近实例</span>
          <span class="section-sub">最近 {{ recentInstances.length }} 次执行</span>
        </div>
      </template>
      <el-table :data="recentInstances" v-loading="loading" size="small" stripe>
        <el-table-column label="实例 ID" width="100">
          <template #default="{ row }">
            <el-link type="primary" :underline="false" @click="openLogs(row.instance_id)">
              {{ row.instance_id }}
            </el-link>
          </template>
        </el-table-column>
        <el-table-column prop="job_name" label="任务名" min-width="160" show-overflow-tooltip />
        <el-table-column prop="app_id" label="项目 ID" min-width="130" show-overflow-tooltip />
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="statusTagType(row.status)" size="small">{{ row.status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="来源" width="90">
          <template #default="{ row }">
            <el-tag size="small" effect="plain" :type="row.source === 'API' ? 'warning' : 'info'">
              {{ row.source || 'CRON' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" min-width="160">
          <template #default="{ row }">{{ formatTime(row.create_time) }}</template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无执行记录" :image-size="80" />
        </template>
      </el-table>
    </el-card>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
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
.tiles {
  margin-bottom: 4px;
}
.tile {
  margin-bottom: 16px;
}
.tile-label {
  font-size: 13px;
  color: #909399;
}
.tile-value {
  margin-top: 6px;
  font-size: 26px;
  font-weight: 600;
  line-height: 1.2;
  color: #303133;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tile-value.addr {
  font-size: 14px;
  font-weight: 500;
}
.tile-value.ok {
  color: #67c23a;
}
.tile-foot {
  margin-top: 8px;
  font-size: 12px;
  color: #909399;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
  line-height: 1.6;
}
.tile-foot.link {
  color: #409eff;
}
.ok {
  color: #67c23a;
}
.bad {
  color: #f56c6c;
}
.section {
  margin-bottom: 16px;
}
.section-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.section-title {
  font-size: 15px;
  font-weight: 600;
  color: #303133;
}
.section-sub {
  font-size: 12px;
  color: #909399;
}
.app-card {
  margin-bottom: 16px;
}
.app-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.app-name {
  font-size: 15px;
  font-weight: 600;
  color: #303133;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.app-id {
  margin-top: 2px;
  font-size: 12px;
  color: #909399;
  word-break: break-all;
}
.app-metrics {
  margin: 12px 0 0;
  padding: 0;
  list-style: none;
}
.app-metrics li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 13px;
  line-height: 2;
}
.m-label {
  color: #909399;
}
.m-value {
  color: #303133;
}
</style>
