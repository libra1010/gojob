<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import { getMe, changePassword } from '@/api/user'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()

const displayName = computed(() => auth.nickname || auth.username || '未登录')
const roleLabel = computed(
  () =>
    ((
      { SUPER_ADMIN: '超级管理员', ADMIN: '管理员', READONLY: '只读' } as Record<string, string>
    )[auth.role] || ''),
)

// 顶层页面不展示面包屑；子页面归到对应父菜单下
const topLevelRoutes = new Set(['overview', 'apps'])
const showCrumb = computed(() => !topLevelRoutes.has(String(route.name)))
const crumbRoot = computed(() => (String(route.name).startsWith('system') ? '/system/users' : '/apps'))
const crumbParent = computed(() => (String(route.name).startsWith('system') ? '系统管理' : '项目管理'))
const activeMenu = computed(() => {
  const n = String(route.name)
  if (n === 'overview') return '/'
  if (n.startsWith('system')) return '/' + n.replace('system-', 'system/')
  return '/apps'
})

function onMenuSelect(index: string) {
  router.push(index)
}

// 进入布局时补齐用户档案（token 有效则成功；失效会被拦截器跳回登录页）
async function refreshProfile() {
  try {
    const me = await getMe()
    auth.setProfile({
      username: me.username,
      nickname: me.nickname,
      role: me.role,
      app_scopes: me.app_scopes,
    })
  } catch {
    /* 拦截器已提示 */
  }
}
if (!auth.username || !auth.role) refreshProfile()

// ---------- 修改密码对话框 ----------
const pwdVisible = ref(false)
const pwdLoading = ref(false)
const pwdFormRef = ref<FormInstance>()
const pwdForm = reactive({ old: '', next: '', confirm: '' })
const pwdRules: FormRules = {
  old: [{ required: true, message: '请输入原密码', trigger: 'blur' }],
  next: [
    { required: true, message: '请输入新密码', trigger: 'blur' },
    { min: 6, message: '至少 6 位', trigger: 'blur' },
  ],
  confirm: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_r, v, cb) => (v === pwdForm.next ? cb() : cb(new Error('两次输入不一致'))),
      trigger: 'blur',
    },
  ],
}
function openPwdDialog() {
  pwdForm.old = pwdForm.next = pwdForm.confirm = ''
  pwdVisible.value = true
}
async function submitPwd() {
  const ok = await pwdFormRef.value?.validate().catch(() => false)
  if (!ok) return
  pwdLoading.value = true
  try {
    await changePassword(pwdForm.old, pwdForm.next)
    pwdVisible.value = false
    ElMessage.success('密码已修改，请重新登录')
    await auth.logout()
    router.push({ name: 'login' })
  } catch {
    /* 拦截器已提示 */
  } finally {
    pwdLoading.value = false
  }
}

async function onLogout() {
  await auth.logout()
  ElMessage.success('已退出登录')
  router.push({ name: 'login' })
}
</script>

<template>
  <el-container class="layout">
    <el-header class="header" height="60px">
      <div class="brand" @click="router.push('/')">
        <span class="logo">G</span>
        <span class="brand-text">
          <span class="title">gojob</span>
          <span class="subtitle">任务调度控制台</span>
        </span>
      </div>
      <el-menu
        class="nav"
        mode="horizontal"
        :default-active="activeMenu"
        :ellipsis="false"
        @select="onMenuSelect"
      >
        <el-menu-item index="/">统计总览</el-menu-item>
        <el-menu-item index="/apps">项目管理</el-menu-item>
        <el-sub-menu v-if="auth.isAdmin" index="/system">
          <template #title>系统管理</template>
          <el-menu-item v-if="auth.isSuperAdmin" index="/system/users">用户管理</el-menu-item>
          <el-menu-item index="/system/login-logs">登录日志</el-menu-item>
          <el-menu-item index="/system/operation-logs">操作日志</el-menu-item>
          <el-menu-item index="/system/nodes">节点监控</el-menu-item>
        </el-sub-menu>
      </el-menu>
      <el-dropdown trigger="click">
        <button class="user" type="button" title="账号菜单">
          <span class="avatar">{{ (displayName || '?').slice(0, 1).toUpperCase() }}</span>
          <span class="user-meta">
            <span class="user-name">{{ displayName }}</span>
            <span class="role-tag">{{ roleLabel }}</span>
          </span>
          <span class="caret">▾</span>
        </button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item @click="openPwdDialog">修改密码</el-dropdown-item>
            <el-dropdown-item divided @click="onLogout">退出登录</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
    </el-header>
    <el-main class="main">
      <div class="main-inner">
        <el-breadcrumb v-if="showCrumb" separator="/" class="crumb">
          <el-breadcrumb-item :to="{ path: crumbRoot }">{{ crumbParent }}</el-breadcrumb-item>
          <el-breadcrumb-item>{{ route.meta.title }}</el-breadcrumb-item>
        </el-breadcrumb>
        <router-view />
      </div>
    </el-main>

    <el-dialog v-model="pwdVisible" title="修改密码" width="420px" append-to-body>
      <el-form ref="pwdFormRef" :model="pwdForm" :rules="pwdRules" label-width="80px">
        <el-form-item label="原密码" prop="old">
          <el-input v-model="pwdForm.old" type="password" show-password autocomplete="current-password" />
        </el-form-item>
        <el-form-item label="新密码" prop="next">
          <el-input v-model="pwdForm.next" type="password" show-password autocomplete="new-password" />
        </el-form-item>
        <el-form-item label="确认密码" prop="confirm">
          <el-input v-model="pwdForm.confirm" type="password" show-password autocomplete="new-password" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="pwdVisible = false">取 消</el-button>
        <el-button type="primary" :loading="pwdLoading" @click="submitPwd">确 定</el-button>
      </template>
    </el-dialog>
  </el-container>
</template>

<style scoped>
.layout {
  min-height: 100vh;
  background: var(--gj-canvas);
}
.header {
  position: sticky;
  top: 0;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 28px;
  padding: 0 24px;
  background: var(--gj-nav);
  border-bottom: 1px solid var(--gj-nav-border);
  box-shadow: 0 1px 0 rgba(8, 16, 28, 0.35);
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  cursor: pointer;
  flex-shrink: 0;
}
.logo {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: 8px;
  background: var(--gj-primary);
  color: #fff;
  font-size: 16px;
  font-weight: 750;
  box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.18);
}
.title {
  font-size: 17px;
  font-weight: 650;
  letter-spacing: 0.01em;
  color: #fff;
  white-space: nowrap;
}
/* 主标题只写产品名，定位放副标题：一屏之内先认出"这是 gojob"，再看它是干什么的 */
.brand-text {
  display: flex;
  flex-direction: column;
  line-height: 1.2;
}
.subtitle {
  font-size: 11px;
  letter-spacing: 0.04em;
  color: #8fa6c2;
  white-space: nowrap;
}
.nav {
  flex: 1;
  min-width: 0;
  border-bottom: none;
  background: transparent;
  --el-menu-item-height: 60px;
  --el-menu-bg-color: transparent;
  --el-menu-text-color: var(--gj-nav-text);
  --el-menu-hover-text-color: #fff;
  --el-menu-hover-bg-color: rgba(255, 255, 255, 0.06);
  --el-menu-active-color: #fff;
  --el-menu-base-level-padding: 16px;
}
.nav :deep(.el-menu-item),
.nav :deep(.el-sub-menu__title) {
  border-bottom: 2px solid transparent;
  font-size: 13px;
  font-weight: 500;
}
.nav :deep(.el-menu-item.is-active) {
  color: #fff;
  border-bottom-color: #60a5fa;
  background: rgba(96, 165, 250, 0.1);
}
.nav :deep(.el-sub-menu.is-active .el-sub-menu__title) {
  color: #fff;
  border-bottom-color: #60a5fa;
}
.user {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 4px 8px 4px 4px;
  border: 1px solid transparent;
  border-radius: 8px;
  background: transparent;
  color: var(--gj-nav-text);
  font: inherit;
  cursor: pointer;
}
.user:hover,
.user:focus-visible {
  background: rgba(255, 255, 255, 0.06);
  border-color: var(--gj-nav-border);
  outline: none;
}
.avatar {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border-radius: 50%;
  background: #1d3554;
  border: 1px solid rgba(148, 176, 214, 0.28);
  color: #dbeafe;
  font-size: 12px;
  font-weight: 600;
}
.user-meta {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  line-height: 1.25;
}
.user-name {
  font-size: 12.5px;
  font-weight: 500;
  color: #fff;
}
.role-tag {
  font-size: 11px;
  color: #8fa6c2;
}
.caret {
  margin-left: 2px;
  font-size: 10px;
  color: #8fa6c2;
}
.main {
  padding: 20px 24px 32px;
}
.main-inner {
  width: 100%;
  max-width: 1560px;
  margin: 0 auto;
}
.crumb {
  margin-bottom: 12px;
}
@media (max-width: 900px) {
  .header {
    gap: 12px;
    padding: 0 16px;
  }
  .subtitle,
  .user-meta {
    display: none;
  }
  .main {
    padding: 16px;
  }
}
@media (max-width: 640px) {
  .brand-text {
    display: none;
  }
}
</style>
