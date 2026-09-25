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
  <el-card shadow="never">
    <div class="toolbar">
      <h3 class="page-title">项目管理</h3>
      <div class="right">
        <span class="count">共 {{ apps.length }} 个项目</span>
        <el-button @click="load" :loading="loading">刷 新</el-button>
        <el-button v-if="auth.canWrite" type="primary" @click="openCreate">新建项目</el-button>
      </div>
    </div>

    <el-empty
      v-if="!apps.length && !loading"
      description="暂无项目，点击右上角「新建项目」创建"
    />
    <el-row v-else v-loading="loading" :gutter="16">
      <el-col v-for="app in apps" :key="app.app_id" :xs="24" :sm="12" :md="8">
        <el-card shadow="hover" class="app-card">
          <div class="app-card-head">
            <el-link
              type="primary"
              :underline="false"
              class="app-name"
              :title="app.app_name || app.app_id"
              @click="router.push(`/apps/${app.app_id}/jobs`)"
            >
              {{ app.app_name || app.app_id }}
            </el-link>
            <el-tag
              v-if="(statByApp.get(app.app_id)?.worker_count ?? 0) > 0"
              type="success"
              size="small"
              effect="plain"
            >
              在线
            </el-tag>
            <el-tag v-else type="info" size="small" effect="plain">离线</el-tag>
          </div>
          <div class="app-id">{{ app.app_id }}</div>

          <ul class="app-metrics">
            <li>
              <span class="m-label">任务数</span>
              <span class="m-value">{{ statByApp.get(app.app_id)?.job_count ?? '-' }}</span>
            </li>
            <li>
              <span class="m-label">在线节点数</span>
              <span class="m-value">
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
            </li>
            <li>
              <span class="m-label">调度副本</span>
              <span class="m-value">
                <template v-if="app.current_server">{{ app.current_server }}</template>
                <span v-else class="sub">—（暂无存活副本持有租约）</span>
              </span>
            </li>
            <li>
              <span class="m-label">创建时间</span>
              <span class="m-value">{{ formatTime(app.create_time) }}</span>
            </li>
          </ul>

          <div class="app-actions">
            <el-button size="small" @click="router.push(`/apps/${app.app_id}/jobs`)">
              查看任务
            </el-button>
            <el-button v-if="auth.isSuperAdmin" size="small" @click="openEdit(app)">编 辑</el-button>
            <el-button size="small" @click="openWorkers(app.app_id)">节点</el-button>
            <el-button v-if="auth.isSuperAdmin" size="small" type="danger" plain @click="onDelete(app)">删除</el-button>
          </div>
        </el-card>
      </el-col>
    </el-row>
  </el-card>

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
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}
.right {
  display: flex;
  align-items: center;
  gap: 12px;
}
.count {
  font-size: 12px;
  color: #909399;
}
.page-title {
  margin: 0;
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
  gap: 8px;
  font-size: 13px;
  line-height: 2;
}
.m-label {
  color: #909399;
  flex-shrink: 0;
}
.m-value {
  color: #303133;
  text-align: right;
}
.sdk-tags {
  display: inline-flex;
  gap: 4px;
  margin-left: 6px;
}
.app-actions {
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid #f0f2f5;
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
</style>
