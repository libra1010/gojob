<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { deleteJob, exportAppJobs, exportJob, exportJobs, listAllJobs, listJobs, operateJob } from '@/api/job'
import { ApiError } from '@/api/http'
import type { JobInfo, JobDTO } from '@/api/types'
import { formatTime } from '@/utils/format'
import JobFormDrawer from '@/components/JobFormDrawer.vue'
import ImportDialog from '@/components/ImportDialog.vue'
import WorkersDrawer from '@/components/WorkersDrawer.vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()
const appId = String(route.params.appId || '')
// 可对该项目做任务写操作（超管或被授权的 ADMIN；导入仅超管）
const canManage = computed(() => auth.canWriteApp(appId))
const canImport = computed(() => auth.isSuperAdmin)

// ---------- 过滤条 + 分页（任务管理页只有任务 ID 与关键字两个过滤器） ----------
const filterJobId = ref('')
const filterKeyword = ref('')
const index = ref(0) // 0 基，与后端 PageResult 同一口径
const pageSize = ref(10)
const total = ref(0)

const jobs = ref<JobInfo[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const page = await listJobs(appId, {
      index: index.value,
      pageSize: pageSize.value,
      jobId: filterJobId.value,
      keyword: filterKeyword.value,
    })
    jobs.value = page.data ?? []
    total.value = page.total_items
    index.value = page.index
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}

/** 建单/编辑抽屉里选「上游任务」要全项目候选，不能被分页切掉 */
const allJobs = ref<JobInfo[]>([])
async function loadAll() {
  try {
    allJobs.value = await listAllJobs(appId)
  } catch {
    /* 拦截器已提示 */
  }
}

function onSearch() {
  index.value = 0
  load()
}
function onReset() {
  filterJobId.value = ''
  filterKeyword.value = ''
  index.value = 0
  load()
}
function onPage(page: number) {
  index.value = page - 1 // el-pagination 是 1 基，后端是 0 基
  load()
}
async function reload() {
  await Promise.all([load(), loadAll()])
}
onMounted(reload)

/** 定时信息列：依赖触发型没有表达式，改成显示它在等哪个上游任务 */
// 与兼容协议控制台同名函数等价：未知值原样显示，别把数据藏起来
function translateExecuteType(t: string) {
  switch (t) {
    case 'STANDALONE':
      return '单机执行'
    case 'BROADCAST':
      return '广播执行'
    case 'MAP':
      return 'Map执行'
    case 'MAP_REDUCE':
      return 'MapReduce 执行'
    case 'WORKFLOW':
      return '工作流'
    default:
      return t || '单机执行'
  }
}

function translateProcessorType(t: string) {
  return t === 'EXTERNAL' ? '外置（动态加载）' : '内建'
}

function exprCell(row: JobInfo): string {
  if (row.time_expression_type === 'DEPENDENCY') {
    const up = allJobs.value.find((j) => j.id === row.upstream_job_id)
    return `← ${up ? `#${up.id} ${up.job_name}` : `#${row.upstream_job_id || '?'}`}`
  }
  return row.time_expression_type === 'API' ? '—' : row.time_expression
}

// ---------- 新建 / 编辑 / 复制 ----------
const formVisible = ref(false)
const editingJob = ref<JobInfo | null>(null)
// 复制 = 把源任务的配置灌进「新建」表单（与兼容协议一致：只回填表单，不落库）
const copySource = ref<JobInfo | null>(null)
function openCreate() {
  editingJob.value = null
  copySource.value = null
  formVisible.value = true
}
function openEdit(job: JobInfo) {
  editingJob.value = job
  copySource.value = null
  formVisible.value = true
}
function openCopy(job: JobInfo) {
  editingJob.value = null
  copySource.value = job
  formVisible.value = true
}

// ---------- 运行 / 启停 / 参数运行 / 删除 ----------
async function onOperate(job: JobInfo, action: 'pause' | 'resume' | 'trigger', params?: string) {
  try {
    await operateJob(job.id, action, params)
    if (action === 'pause') ElMessage.success(`任务「${job.job_name}」已暂停`)
    if (action === 'resume') ElMessage.success(`任务「${job.job_name}」已启用`)
    if (action === 'trigger') ElMessage.success(`已触发「${job.job_name}」，可在运行记录查看`)
    load()
  } catch (e) {
    // 手动触发时任务执行中且开启"执行中不发起新任务"：409，给出友好中文提示
    if (action === 'trigger' && e instanceof ApiError && e.code === 409) {
      ElMessage.warning('任务正在执行中，已开启「任务执行中则不发起新任务」，本次触发被跳过')
    }
    /* 其余错误拦截器已提示 */
  }
}

/** 状态列开关：只有后端确认成功才改本地值，失败时开关留在原位，不能显示假状态 */
async function onToggle(job: JobInfo, next: boolean) {
  try {
    await operateJob(job.id, next ? 'resume' : 'pause')
    job.enable = next
    ElMessage.success(next ? `任务「${job.job_name}」已启用` : `任务「${job.job_name}」已暂停`)
  } catch {
    /* 拦截器已提示；开关保持原值 */
  }
}

async function onRunByParams(job: JobInfo) {
  let value: { value: string }
  try {
    value = await ElMessageBox.prompt(
      '本次运行使用的实例参数（JSON）；留空则用任务里配的默认实例参数',
      `参数运行 · ${job.job_name}`,
      {
        inputType: 'textarea',
        inputValue: job.instance_params || '',
        confirmButtonText: '运 行',
        cancelButtonText: '取 消',
      },
    )
  } catch {
    return
  }
  const text = (value.value || '').trim()
  if (text) {
    try {
      JSON.parse(text)
    } catch {
      ElMessage.error('实例参数需为合法 JSON')
      return
    }
  }
  await onOperate(job, 'trigger', text)
}

// ---------- 导出：兼容格式的任务包（JSON 数组），可存文件也可粘回「导入」框 ----------
const exportVisible = ref(false)
const exporting = ref(false)
const exportScope = ref<'single' | 'selected' | 'app'>('single')
const exportJobRef = ref<JobInfo | null>(null)
const exportText = ref('')
const exportName = ref('')
const exportCount = ref(0)
const selection = ref<JobInfo[]>([])

async function loadExport() {
  exporting.value = true
  try {
    let dtos: JobDTO[]
    let name: string
    if (exportScope.value === 'single' && exportJobRef.value) {
      dtos = [await exportJob(exportJobRef.value.id)]
      name = exportJobRef.value.job_name
    } else if (exportScope.value === 'selected') {
      dtos = await exportJobs(selection.value.map((j) => j.id))
      name = `${appId}_选中${dtos.length}个任务`
    } else {
      dtos = await exportAppJobs(appId)
      name = `${appId}_全部${dtos.length}个任务`
    }
    exportName.value = name
    exportCount.value = dtos.length
    exportText.value = JSON.stringify(dtos, null, 2)
  } catch {
    /* 拦截器已提示 */
  } finally {
    exporting.value = false
  }
}

function openExport(job: JobInfo) {
  exportJobRef.value = job
  exportScope.value = 'single'
  exportVisible.value = true
  loadExport()
}

/** 工具栏「批量导出」：默认导选中项，没选中就直接导本项目全部 */
function openBatchExport() {
  exportJobRef.value = null
  exportScope.value = selection.value.length ? 'selected' : 'app'
  exportVisible.value = true
  loadExport()
}

function onExportScopeChange() {
  loadExport()
}

async function copyExport() {
  try {
    await navigator.clipboard.writeText(exportText.value)
    ElMessage.success('导出 JSON 已复制到剪贴板')
  } catch {
    ElMessage.warning('浏览器不允许直接复制，请在文本框里手动全选')
  }
}
function downloadExport() {
  const url = URL.createObjectURL(new Blob([exportText.value], { type: 'application/json' }))
  const a = document.createElement('a')
  a.href = url
  // 任务名可能带 / : 等字符，落盘文件名只留安全字符
  a.download = `${exportName.value.replace(/[\\/:*?"<>|\s]+/g, '_') || 'job'}.json`
  a.click()
  URL.revokeObjectURL(url)
}

async function onDelete(job: JobInfo) {
  try {
    await ElMessageBox.confirm(
      `确定删除任务「${job.job_name}」？历史执行实例将保留以便审计。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删 除', cancelButtonText: '取 消' },
    )
  } catch {
    return
  }
  try {
    await deleteJob(job.id)
    ElMessage.success('任务已删除')
    reload()
  } catch {
    /* 有运行中实例返回 409，拦截器已提示 */
  }
}

// ---------- 导入 / 在线机器 ----------
const importVisible = ref(false)
const workersVisible = ref(false)
</script>

<template>
  <div class="jobs-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <button type="button" class="gj-back" @click="router.push('/apps')">‹ 返回项目</button>
        <h3 class="gj-page-title">
          任务列表
          <span class="app-chip gj-mono">{{ appId }}</span>
        </h3>
        <div class="gj-page-sub">共 {{ total }} 个任务 · 定时表达式与下次触发时间以服务器时区为准</div>
      </div>
      <div class="gj-page-actions">
        <el-button size="small" :loading="loading" @click="reload">刷 新</el-button>
        <el-button size="small" @click="workersVisible = true">在线机器</el-button>
        <el-button v-if="canImport" size="small" @click="importVisible = true">导 入</el-button>
        <el-button v-if="canManage" size="small" @click="openBatchExport">
          批量导出{{ selection.length ? `（已选 ${selection.length}）` : '' }}
        </el-button>
        <el-button v-if="canManage" size="small" type="primary" @click="openCreate">
          新建任务
        </el-button>
      </div>
    </div>

    <el-card shadow="never" class="table-card">
      <div class="filter-bar">
        <div class="filter-inputs">
          <el-input
            v-model="filterJobId"
            placeholder="任务 ID"
            class="f-id"
            clearable
            @keyup.enter="onSearch"
          />
          <el-input
            v-model="filterKeyword"
            placeholder="关键字（任务名 / 描述）"
            class="f-kw"
            clearable
            @keyup.enter="onSearch"
          />
        </div>
        <div class="filter-actions">
          <span class="count-hint">共 {{ total }} 个任务</span>
          <el-button type="primary" plain size="small" @click="onSearch">查 询</el-button>
          <el-button size="small" @click="onReset">重 置</el-button>
        </div>
      </div>

      <el-table :data="jobs" v-loading="loading" size="small" @selection-change="(rows: JobInfo[]) => (selection = rows)">
        <el-table-column type="selection" width="46" />
        <el-table-column label="ID" width="72">
          <template #default="{ row }"><span class="gj-mono">#{{ row.id }}</span></template>
        </el-table-column>
        <el-table-column prop="job_name" label="任务名称" min-width="136" show-overflow-tooltip />
        <el-table-column label="描述" min-width="110" show-overflow-tooltip>
          <template #default="{ row }">
            <span v-if="row.job_description">{{ row.job_description }}</span>
            <span v-else class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="处理器" min-width="158" show-overflow-tooltip>
          <template #default="{ row }">
            <div class="proc-name">{{ row.processor }}</div>
            <div class="proc-type">{{ translateProcessorType(row.processor_type) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="定时信息" min-width="176">
          <template #default="{ row }">
            <div class="time-cell">
              <el-tag size="small" effect="plain" class="type-tag">
                {{ row.time_expression_type || 'CRON' }}
              </el-tag>
              <code class="cron" :title="exprCell(row)">{{ exprCell(row) }}</code>
            </div>
          </template>
        </el-table-column>
        <!-- 列与翻译函数都按兼容协议的任务列表那一版：定时信息 → 执行类型 → 处理器 -->
        <el-table-column label="执行类型" width="92">
          <template #default="{ row }">{{ translateExecuteType(row.execute_type) }}</template>
        </el-table-column>
        <el-table-column label="状态" width="104">
          <template #default="{ row }">
            <div class="status-cell">
              <!-- 启用/暂停就是一个开关，不另设按钮（同兼容协议） -->
              <el-switch
                :model-value="row.enable"
                :disabled="!canManage"
                size="small"
                :aria-label="`任务「${row.job_name}」启用状态`"
                style="--el-switch-on-color: var(--gj-success); --el-switch-off-color: var(--gj-warning)"
                @update:model-value="(v: any) => onToggle(row, !!v)"
              />
              <el-tooltip
                v-if="row.skip_when_running"
                content="任务执行中则不发起新任务"
                placement="top"
              >
                <span class="skip-mark">跳过</span>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="日志级别" width="84">
          <template #default="{ row }">
            <el-tag size="small" effect="plain" type="info">{{ row.log_level || 'INFO' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="下次触发时间" min-width="160">
          <template #default="{ row }">
            <span v-if="row.time_expression_type === 'API'" class="muted">手动触发</span>
            <span v-else-if="row.time_expression_type === 'DEPENDENCY'" class="muted">
              等待上游触发
            </span>
            <span v-else class="time">{{ formatTime(row.next_trigger_time) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="204" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
              <el-button
                v-if="canManage"
                link
                type="primary"
                size="small"
                @click="onOperate(row, 'trigger')"
              >
                运行
              </el-button>
              <el-button v-if="canManage" link type="primary" size="small" @click="openEdit(row)">
                编辑
              </el-button>
              <el-button link size="small" @click="router.push(`/jobs/${row.id}/instances`)">
                记录
              </el-button>
              <el-dropdown v-if="canManage" trigger="click" class="more">
                <el-button link type="primary" size="small">更多 ▾</el-button>
                <template #dropdown>
                  <el-dropdown-menu>
                    <el-dropdown-item @click="onRunByParams(row)">参数运行</el-dropdown-item>
                    <el-dropdown-item @click="openCopy(row)">复 制</el-dropdown-item>
                    <el-dropdown-item @click="openExport(row)">导 出</el-dropdown-item>
                    <el-dropdown-item divided @click="onDelete(row)">
                      <span class="danger">删 除</span>
                    </el-dropdown-item>
                  </el-dropdown-menu>
                </template>
              </el-dropdown>
            </div>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="该项目暂无任务" :image-size="72" />
        </template>
      </el-table>

      <!-- 后端 index 是 0 基，这里转成 1 基；只有一页时不占地方 -->
      <el-pagination
        class="pager"
        layout="prev, pager, next, total"
        :total="total"
        :page-size="pageSize"
        :current-page="index + 1"
        :hide-on-single-page="true"
        @current-change="onPage"
      />
    </el-card>
  </div>

  <JobFormDrawer
    v-model="formVisible"
    :app-id="appId"
    :job="editingJob"
    :copy-from="copySource"
    :jobs="allJobs"
    @saved="reload"
  />
  <ImportDialog v-model="importVisible" :app-id="appId" @imported="reload" />
  <WorkersDrawer v-model="workersVisible" :app-id="appId" />

  <el-dialog v-model="exportVisible" title="导出任务" width="680px">
    <el-alert type="info" :closable="false" class="tip">
      导出的是通用任务包格式（JobDTO 数组，camelCase）：可存成 .json 文件，也可以整段粘回「导入」框，
      兼容 PowerJob 控制台导出的任务包同样能被本页面导入。
    </el-alert>
    <el-radio-group v-model="exportScope" class="scope" @change="onExportScopeChange">
      <el-radio-button v-if="exportJobRef" value="single">单个任务（{{ exportJobRef.job_name }}）</el-radio-button>
      <el-radio-button value="selected" :disabled="!selection.length">
        选中任务（{{ selection.length }} 个）
      </el-radio-button>
      <el-radio-button value="app">本项目全部（{{ total }} 个）</el-radio-button>
    </el-radio-group>
    <div class="tools">
      <el-button size="small" :disabled="!exportText" @click="copyExport">复制 JSON</el-button>
      <el-button size="small" :disabled="!exportText" @click="downloadExport">下载 .json</el-button>
      <el-button size="small" :loading="exporting" @click="loadExport">重 取</el-button>
      <span class="hint">共 {{ exportCount }} 条 · {{ exportText.length }} 字节</span>
    </div>
    <el-input v-model="exportText" type="textarea" :rows="16" readonly class="mono" v-loading="exporting" />
    <template #footer>
      <el-button type="primary" @click="exportVisible = false">关 闭</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.app-chip {
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
.table-card :deep(.el-card__body) {
  padding: 14px 16px 12px;
}
.filter-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
  margin-bottom: 12px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--gj-border-soft);
}
.filter-inputs {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.filter-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.count-hint {
  margin-right: 4px;
  font-size: 12px;
  color: var(--gj-text-muted);
}
.f-id {
  width: 130px;
}
.f-kw {
  width: 250px;
}
.time-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  white-space: nowrap;
}
.type-tag {
  flex-shrink: 0;
}
.cron {
  max-width: 150px;
  padding: 1px 6px;
  border: 1px solid var(--gj-border-soft);
  border-radius: 4px;
  background: var(--gj-surface-subtle);
  color: var(--gj-text-regular);
  font-family: var(--gj-mono);
  font-size: 11.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.time {
  font-size: 12px;
  color: var(--gj-text-muted);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.proc-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.proc-type {
  margin-top: 1px;
  font-size: 11px;
  color: var(--gj-text-faint);
}
.status-cell {
  display: flex;
  align-items: center;
  gap: 8px;
}
.skip-mark {
  padding: 0 5px;
  border-radius: 3px;
  background: var(--gj-warning-soft);
  color: var(--gj-warning);
  font-size: 11px;
  line-height: 17px;
  cursor: help;
}
.row-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}
.row-actions :deep(.el-button + .el-button) {
  margin-left: 0;
}
.row-actions :deep(.el-button) {
  padding: 0;
  height: auto;
}
.row-actions :deep(.el-button.is-link) {
  font-size: 12px;
}
.more {
  vertical-align: middle;
}
.pager {
  margin-top: 12px;
  justify-content: flex-end;
}
.danger {
  color: var(--gj-danger);
}
.tip {
  margin-bottom: 10px;
}
.tools {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.scope {
  margin-bottom: 10px;
}
.hint {
  font-size: 12px;
  color: var(--gj-text-muted);
}
.mono :deep(textarea) {
  font-family: var(--gj-mono);
  font-size: 12px;
  line-height: 1.55;
}
.muted {
  color: var(--gj-text-faint);
}
@media (max-width: 900px) {
  .f-kw {
    width: 200px;
  }
}
</style>
