<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  listUsers,
  createUser,
  modifyUser,
  resetPassword,
  setUserStatus,
  deleteUser,
} from '@/api/user'
import { listApps } from '@/api/app'
import { formatTime } from '@/utils/format'
import type { Role, UserInfo, UserReq } from '@/api/types'

const loading = ref(false)
const users = ref<UserInfo[]>([])
const appOptions = ref<string[]>([])

const roleLabel: Record<Role, string> = {
  SUPER_ADMIN: '超级管理员',
  ADMIN: '管理员',
  READONLY: '只读',
}
const roleTagType: Record<Role, 'primary' | 'warning' | 'info'> = {
  SUPER_ADMIN: 'primary',
  ADMIN: 'warning',
  READONLY: 'info',
}

const activeUsers = computed(() => users.value.filter((u) => u.status === 'ACTIVE').length)

async function load() {
  loading.value = true
  try {
    users.value = await listUsers()
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await load()
  try {
    const apps = await listApps()
    appOptions.value = apps.map((a) => a.app_id)
  } catch {
    /* 拦截器已提示 */
  }
})

// ---------- 新建 / 编辑 ----------
const dialogVisible = ref(false)
const isEdit = ref(false)
const saving = ref(false)
const form = reactive<UserReq>({
  username: '',
  password: '',
  nickname: '',
  role: 'READONLY',
  app_scopes: [],
})

function openCreate() {
  isEdit.value = false
  Object.assign(form, { username: '', password: '', nickname: '', role: 'READONLY', app_scopes: [] })
  dialogVisible.value = true
}
function openEdit(u: UserInfo) {
  isEdit.value = true
  Object.assign(form, {
    username: u.username,
    password: '',
    nickname: u.nickname,
    role: u.role,
    app_scopes: u.app_scopes.filter((s) => s !== '*'),
  })
  dialogVisible.value = true
}
async function save() {
  if (!form.username) return ElMessage.warning('请填写用户名')
  if (!isEdit.value && !form.password) return ElMessage.warning('请设置初始密码')
  saving.value = true
  try {
    if (isEdit.value) {
      await modifyUser({ ...form, password: undefined })
    } else {
      await createUser({ ...form })
    }
    dialogVisible.value = false
    ElMessage.success('已保存')
    load()
  } catch {
    /* 拦截器已提示 */
  } finally {
    saving.value = false
  }
}

async function onResetPwd(u: UserInfo) {
  try {
    const { value } = await ElMessageBox.prompt(`为 ${u.username} 设置新密码`, '重置密码', {
      inputType: 'password',
      inputValidator: (v) => (v && v.length >= 6 ? true : '至少 6 位'),
    })
    await resetPassword(u.username, value)
    ElMessage.success('密码已重置，该用户需重新登录')
  } catch {
    /* 取消 */
  }
}

async function onToggleStatus(u: UserInfo) {
  const disable = u.status === 'ACTIVE'
  await setUserStatus(u.username, disable ? 'DISABLED' : 'ACTIVE')
  ElMessage.success(disable ? '已禁用并踢下线' : '已启用')
  load()
}

async function onDelete(u: UserInfo) {
  await ElMessageBox.confirm(`确认删除用户 ${u.username}？`, '删除', { type: 'warning' })
  await deleteUser(u.username)
  ElMessage.success('已删除')
  load()
}
</script>

<template>
  <div class="users-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <h3 class="gj-page-title">用户管理</h3>
        <div class="gj-page-sub">
          <span>共 {{ users.length }} 个账号</span>
          <span class="meta-sep" />
          <span>启用 {{ activeUsers }} 个</span>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-button size="small" :loading="loading" @click="load">刷 新</el-button>
        <el-button size="small" type="primary" @click="openCreate">新建用户</el-button>
      </div>
    </div>

    <el-card shadow="never" class="table-card">
      <el-table v-loading="loading" :data="users" size="small">
        <el-table-column label="ID" width="68">
          <template #default="{ row }"><span class="gj-mono">#{{ row.id }}</span></template>
        </el-table-column>
        <el-table-column prop="username" label="用户名" width="140">
          <template #default="{ row }"><span class="username">{{ row.username }}</span></template>
        </el-table-column>
        <el-table-column prop="nickname" label="昵称" width="140" />
        <el-table-column label="角色" width="116">
          <template #default="{ row }">
            <el-tag :type="roleTagType[row.role as Role]" size="small" effect="plain">
              {{ roleLabel[row.role as Role] }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="92">
          <template #default="{ row }">
            <span
              class="gj-pill"
              :class="row.status === 'ACTIVE' ? 'gj-pill--ok' : 'gj-pill--bad'"
            >{{ row.status === 'ACTIVE' ? '启用' : '禁用' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="可操作项目" min-width="200">
          <template #default="{ row }">
            <span v-if="row.role === 'SUPER_ADMIN'">全部</span>
            <span v-else-if="row.role === 'READONLY'" class="muted">只读（不可写）</span>
            <template v-else>
              <el-tag v-for="s in row.app_scopes" :key="s" size="small" class="scope-tag">
                {{ s }}
              </el-tag>
              <span v-if="!row.app_scopes.length" class="muted">未授权</span>
            </template>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="166">
          <template #default="{ row }"><span class="time">{{ formatTime(row.create_time) }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="272" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
              <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
              <el-button link type="primary" @click="onResetPwd(row)">重置密码</el-button>
              <el-button
                link
                :type="row.status === 'ACTIVE' ? 'warning' : 'success'"
                @click="onToggleStatus(row)"
              >
                {{ row.status === 'ACTIVE' ? '禁用' : '启用' }}
              </el-button>
              <el-button link type="danger" @click="onDelete(row)">删除</el-button>
            </div>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="dialogVisible" :title="isEdit ? '编辑用户' : '新建用户'" width="480px">
      <el-form :model="form" label-width="96px">
        <el-form-item label="用户名" required>
          <el-input v-model="form.username" :disabled="isEdit" placeholder="登录账号" />
        </el-form-item>
        <el-form-item label="昵称">
          <el-input v-model="form.nickname" placeholder="显示名" />
        </el-form-item>
        <el-form-item v-if="!isEdit" label="初始密码" required>
          <el-input v-model="form.password" type="password" show-password placeholder="至少 6 位" />
        </el-form-item>
        <el-form-item label="角色">
          <el-select v-model="form.role" style="width: 100%">
            <el-option label="超级管理员" value="SUPER_ADMIN" />
            <el-option label="管理员（按项目授权）" value="ADMIN" />
            <el-option label="只读" value="READONLY" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="form.role === 'ADMIN'" label="授权项目">
          <el-select v-model="form.app_scopes" multiple style="width: 100%" placeholder="选择可管理的项目">
            <el-option v-for="a in appOptions" :key="a" :label="a" :value="a" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取 消</el-button>
        <el-button type="primary" :loading="saving" @click="save">确 定</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.meta-sep {
  width: 1px;
  height: 11px;
  background: var(--gj-border);
}
.table-card :deep(.el-card__body) {
  padding: 6px 16px 10px;
}
.username {
  font-weight: 500;
  color: var(--gj-text);
}
.scope-tag {
  margin-right: 4px;
}
.time {
  font-size: 12px;
  color: var(--gj-text-muted);
  font-variant-numeric: tabular-nums;
}
.row-actions {
  display: flex;
  align-items: center;
  gap: 2px;
}
.muted {
  color: var(--gj-text-faint);
}
</style>
