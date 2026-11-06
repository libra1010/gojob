<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { createApp as createAppApi, deleteApp, listApps, modifyApp } from '@/api/app'
import { getStatsOverview } from '@/api/stats'
import type { AppInfo, AppStat, StatsOverview, WorkerStat } from '@/api/types'
import { formatTime, sdkTagType } from '@/utils/format'
import WorkersDrawer from '@/components/WorkersDrawer.vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()
const apps = ref<AppInfo[]>([])
const loading = ref(false)

/** 总览数据仅用于补齐卡片上的任务数/在线节点数（增删改仍以 /app/list 为准） */
const stats = ref<StatsOverview | null>(null)

const statByApp = computed(() => {
  const m = new Map<string, AppStat>()
  for (const a of stats.value?.apps ?? []) m.set(a.app_id, a)
  return m
})

const workersByApp = computed(() => {
  const m = new Map<string, WorkerStat[]>()
  for (const w of stats.value?.workers ?? []) {
    const list = m.get(w.app_id)
    if (list) list.push(w)
    else m.set(w.app_id, [w])
  }
  return m
})

/** 有在线节点心跳的项目数，用于页头概览 */
const onlineApps = computed(
  () =>
    apps.value.filter((a) => (statByApp.value.get(a.app_id)?.worker_count ?? 0) > 0).length,
)

function isOnline(appId: string) {
  return (statByApp.value.get(appId)?.worker_count ?? 0) > 0
}
function appInitial(app: AppInfo) {
  return (app.app_name || app.app_id).slice(0, 1).toUpperCase()
}

/** 该项目的 SDK 类型分布，如 [ [ 'GO', 2 ], [ 'JAVA', 1 ] ] */
function sdkBreakdown(appId: string): Array<[string, number]> {
  const counts = new Map<string, number>()
  for (const w of workersByApp.value.get(appId) ?? []) {
    counts.set(w.sdk_type, (counts.get(w.sdk_type) ?? 0) + 1)
  }
  return [...counts.entries()].sort((a, b) => b[1] - a[1])
}

async function load() {
  loading.value = true
  try {
    apps.value = await listApps()
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
  try {
    stats.value = await getStatsOverview()
  } catch {
    /* 统计失败不影响项目卡片渲染 */
  }
}
onMounted(load)

// ---------- 新建 / 编辑弹窗 ----------
const dialogVisible = ref(false)
const editing = ref(false)
const submitting = ref(false)
const formRef = ref<FormInstance>()
const form = reactive({ app_id: '', app_name: '', password: '' })

const rules: FormRules = {
  app_id: [
    { required: true, message: '请输入项目 ID', trigger: 'blur' },
    { pattern: /^[A-Za-z0-9_.-]+$/, message: '仅允许字母、数字、下划线、中划线、点', trigger: 'blur' },
  ],
  app_name: [{ required: true, message: '请输入项目名称', trigger: 'blur' }],
  password: [{ validator: validatePassword, trigger: 'blur' }],
}

function validatePassword(_rule: unknown, value: string, callback: (e?: Error) => void) {
  if (!editing.value && !value) {
    callback(new Error('请输入项目密码'))
    return
  }
  callback()
}

function openCreate() {
  editing.value = false
  Object.assign(form, { app_id: '', app_name: '', password: '' })
  dialogVisible.value = true
}

function openEdit(app: AppInfo) {
  editing.value = true
  Object.assign(form, { app_id: app.app_id, app_name: app.app_name, password: '' })
  dialogVisible.value = true
}

async function onSubmit() {
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return
  submitting.value = true
  try {
    if (editing.value) {
      // 密码留空 = 不修改
      const req: Parameters<typeof modifyApp>[0] = { app_id: form.app_id, app_name: form.app_name }
      if (form.password) req.password = form.password
      await modifyApp(req)
      ElMessage.success('项目已更新')
    } else {
      await createAppApi({ app_id: form.app_id, app_name: form.app_name, password: form.password })
      ElMessage.success('项目已创建')
    }
    dialogVisible.value = false
    load()
  } catch {
    /* 拦截器已提示 */
  } finally {
    submitting.value = false
  }
}

// ---------- 删除：有任务时后端返回 409，message 由拦截器弹出 ----------
async function onDelete(app: AppInfo) {
  try {
    await ElMessageBox.confirm(
      `确定删除项目「${app.app_name || app.app_id}」？该操作不可恢复。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删 除', cancelButtonText: '取 消' },
    )
  } catch {
    return
  }
  try {
    await deleteApp(app.app_id)
    ElMessage.success('项目已删除')
    load()
  } catch {
    /* 409「app still has N job(s)」等信息已由拦截器提示 */
  }
}

// ---------- 在线机器 ----------
const workersVisible = ref(false)
const workersAppId = ref('')
function openWorkers(appId: string) {
  workersAppId.value = appId
  workersVisible.value = true
}
</script>

<template>
  <div class="apps-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <h3 class="gj-page-title">项目管理</h3>
        <div class="gj-page-sub">
          <span>共 {{ apps.length }} 个项目</span>
          <span class="meta-sep" />
          <span class="online-text"><i class="online-dot" />{{ onlineApps }} 个有在线节点</span>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-button size="small" :loading="loading" @click="load">刷 新</el-button>
        <el-button v-if="auth.canWrite" size="small" type="primary" @click="openCreate">
          新建项目
        </el-button>
      </div>
    </div>

    <div v-loading="loading" class="apps-body">
      <el-empty
        v-if="!apps.length && !loading"
        description="暂无项目，点击右上角「新建项目」创建"
      />
      <div v-else class="app-grid">
        <el-card v-for="app in apps" :key="app.app_id" shadow="never" class="app-card">
          <div class="app-card-head">
            <span class="app-avatar">{{ appInitial(app) }}</span>
            <div class="app-identity">
              <div class="app-title-row">
                <button
                  type="button"
                  class="app-name"
                  :title="app.app_name || app.app_id"
                  @click="router.push(`/apps/${app.app_id}/jobs`)"
                >
                  {{ app.app_name || app.app_id }}
                </button>
                <span
                  class="gj-pill"
                  :class="isOnline(app.app_id) ? 'gj-pill--ok' : 'gj-pill--idle'"
                >
                  {{ isOnline(app.app_id) ? '在线' : '离线' }}
                </span>
              </div>
              <div class="app-id gj-mono">{{ app.app_id }}</div>
            </div>
          </div>

          <div class="app-metrics">
            <div class="metric">
              <span class="m-label">任务</span>
              <span class="m-value gj-num">{{ statByApp.get(app.app_id)?.job_count ?? '-' }}</span>
            </div>
            <div class="metric">
              <span class="m-label">在线节点</span>
              <span class="m-value gj-num">
                {{ statByApp.get(app.app_id)?.worker_count ?? '-' }}
                <span v-if="sdkBreakdown(app.app_id).length" class="sdk-tags">
                  <el-tag
                    v-for="[sdk, n] in sdkBreakdown(app.app_id)"
                    :key="sdk"
                    :type="sdkTagType(sdk)"
                    size="small"
                    effect="plain"
                  >
                    {{ sdk }} × {{ n }}
                  </el-tag>
                </span>
              </span>
            </div>
            <div class="metric metric-wide">
              <span class="m-label">调度副本</span>
              <span
                v-if="app.current_server"
                class="m-value replica gj-mono"
                :title="app.current_server"
              >{{ app.current_server }}</span>
              <span v-else class="m-value replica-empty">暂无存活副本持有租约</span>
            </div>
          </div>

          <div class="app-foot">
            <div class="app-actions">
              <el-button
                size="small"
                type="primary"
                plain
                @click="router.push(`/apps/${app.app_id}/jobs`)"
              >
                查看任务
              </el-button>
              <el-button v-if="auth.isSuperAdmin" size="small" @click="openEdit(app)">编 辑</el-button>
              <el-button size="small" @click="openWorkers(app.app_id)">节点</el-button>
              <el-button
                v-if="auth.isSuperAdmin"
                size="small"
                type="danger"
                plain
                @click="onDelete(app)"
              >删除</el-button>
            </div>
            <div class="app-time">创建于 {{ formatTime(app.create_time) }}</div>
          </div>
        </el-card>
      </div>
    </div>
  </div>

  <el-dialog v-model="dialogVisible" :title="editing ? '编辑项目' : '新建项目'" width="460px">
    <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
      <el-form-item label="项目 ID" prop="app_id">
        <el-input v-model="form.app_id" :disabled="editing" placeholder="如 demo_app" />
      </el-form-item>
      <el-form-item label="项目名称" prop="app_name">
        <el-input v-model="form.app_name" placeholder="展示用名称" />
      </el-form-item>
      <el-form-item label="项目密码" prop="password">
        <el-input
          v-model="form.password"
          type="password"
          show-password
          :placeholder="editing ? '留空表示不修改密码' : 'Worker 心跳鉴权使用'"
        />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="dialogVisible = false">取 消</el-button>
      <el-button type="primary" :loading="submitting" @click="onSubmit">确 定</el-button>
    </template>
  </el-dialog>

  <WorkersDrawer v-model="workersVisible" :app-id="workersAppId" />
</template>

<style scoped>
.meta-sep {
  width: 1px;
  height: 11px;
  background: var(--gj-border);
}
.online-text {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.online-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--gj-success);
}
.apps-body {
  min-height: 160px;
}

.app-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(330px, 1fr));
  gap: 16px;
}
.app-card {
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.app-card:hover {
  border-color: #c9d8ec;
  box-shadow: 0 10px 24px -20px rgba(16, 24, 40, 0.4);
}
.app-card-head {
  display: flex;
  align-items: flex-start;
  gap: 10px;
}
.app-avatar {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  flex-shrink: 0;
  border-radius: 8px;
  background: var(--gj-primary-soft);
  color: var(--gj-primary);
  font-size: 15px;
  font-weight: 650;
}
.app-identity {
  flex: 1;
  min-width: 0;
}
.app-title-row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.app-name {
  min-width: 0;
  padding: 0;
  border: 0;
  background: none;
  color: var(--gj-text);
  font: inherit;
  font-size: 15px;
  font-weight: 600;
  text-align: left;
  cursor: pointer;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.app-name:hover {
  color: var(--gj-primary);
}
.app-name:focus-visible {
  outline: 2px solid var(--gj-primary);
  outline-offset: 2px;
  border-radius: 3px;
}
.app-id {
  margin-top: 3px;
  font-size: 11px;
  color: var(--gj-text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.app-metrics {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px 14px;
  margin-top: 14px;
  padding: 12px 0;
  border-top: 1px solid var(--gj-border-soft);
  border-bottom: 1px solid var(--gj-border-soft);
}
.metric {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.metric-wide {
  grid-column: 1 / -1;
}
.m-label {
  font-size: 11px;
  color: var(--gj-text-muted);
}
.m-value {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--gj-text);
}
.replica {
  display: block;
  font-size: 12px;
  font-weight: 500;
  color: var(--gj-text-regular);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.replica-empty {
  font-size: 12px;
  font-weight: 400;
  color: var(--gj-text-faint);
}
.sdk-tags {
  display: inline-flex;
  flex-wrap: wrap;
  gap: 4px;
}

.app-foot {
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 8px;
  margin-top: 12px;
}
.app-time {
  font-size: 11px;
  color: var(--gj-text-faint);
}
.app-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.app-actions :deep(.el-button + .el-button) {
  margin-left: 0;
}

@media (max-width: 720px) {
  .app-grid {
    grid-template-columns: 1fr;
  }
}
@media (prefers-reduced-motion: reduce) {
  .app-card {
    transition: none;
  }
}
</style>
