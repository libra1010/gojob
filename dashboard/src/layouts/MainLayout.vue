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
    <el-header class="header">
      <div class="brand" @click="router.push('/')">
        <span class="logo">G</span>
        <span class="brand-text">
          <span class="title">gojob</span>
          <span class="subtitle">轻量级分布式任务调度平台</span>
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
        <span class="user">{{ displayName }}<span class="role-tag">{{ roleLabel }}</span></span>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item @click="openPwdDialog">修改密码</el-dropdown-item>
            <el-dropdown-item divided @click="onLogout">退出登录</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
    </el-header>
    <el-main>
      <el-breadcrumb v-if="showCrumb" separator="/" class="crumb">
        <el-breadcrumb-item :to="{ path: crumbRoot }">{{ crumbParent }}</el-breadcrumb-item>
        <el-breadcrumb-item>{{ route.meta.title }}</el-breadcrumb-item>
      </el-breadcrumb>
      <router-view />
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
}
.header {
  display: flex;
  align-items: center;
  gap: 28px;
  background: #fff;
  border-bottom: 1px solid #e4e7ed;
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
  width: 30px;
  height: 30px;
  border-radius: 6px;
  background: #409eff;
  color: #fff;
  font-weight: 700;
}
.title {
  font-size: 17px;
  font-weight: 600;
  color: #303133;
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
  color: #909399;
  white-space: nowrap;
}
.nav {
  flex: 1;
  border-bottom: none;
  background: transparent;
  --el-menu-item-height: 58px;
}
.user {
  cursor: pointer;
  color: #606266;
  white-space: nowrap;
}
.role-tag {
  margin-left: 6px;
  font-size: 12px;
  color: #909399;
}
.crumb {
  margin-bottom: 14px;
}
</style>
