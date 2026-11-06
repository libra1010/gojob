<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()

const formRef = ref<FormInstance>()
const loading = ref(false)
const form = reactive({ username: 'admin', password: '' })

const rules: FormRules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

async function onSubmit() {
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return
  loading.value = true
  try {
    await auth.login(form.username, form.password)
    ElMessage.success('登录成功')
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
    router.replace(redirect)
  } catch {
    // 拦截器已弹出错误提示
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page">
    <div class="shell">
      <aside class="aside">
        <div class="aside-top">
          <span class="logo">G</span>
          <span class="wordmark">gojob</span>
        </div>

        <div class="aside-copy">
          <h1>任务调度控制台</h1>
          <p class="lede">轻量级分布式任务调度平台 · 单二进制 · 零中间件</p>
          <ul class="points">
            <li>
              <span class="dot" />
              <div>
                <strong>单进程部署</strong>
                <span>调度中心与 Worker 注册同源，无需额外中间件</span>
              </div>
            </li>
            <li>
              <span class="dot" />
              <div>
                <strong>多种触发方式</strong>
                <span>定时表达式、固定频率、固定延迟与手动运行</span>
              </div>
            </li>
            <li>
              <span class="dot" />
              <div>
                <strong>执行过程可追溯</strong>
                <span>实例状态、分片任务与实时日志集中查看</span>
              </div>
            </li>
          </ul>
        </div>

        <div class="aside-foot">
          <span>gojob v0.2.0</span>
          <span class="sep" />
          <span>内部运维系统</span>
        </div>
      </aside>

      <section class="panel">
        <div class="panel-inner">
          <header class="panel-head">
            <h2>登录控制台</h2>
            <p>使用管理员分配的账号进入管理端</p>
          </header>

          <el-form
            ref="formRef"
            :model="form"
            :rules="rules"
            label-position="top"
            size="large"
            @submit.prevent="onSubmit"
          >
            <el-form-item label="用户名" prop="username">
              <el-input v-model="form.username" placeholder="请输入用户名" autocomplete="username" />
            </el-form-item>
            <el-form-item label="密码" prop="password">
              <el-input
                v-model="form.password"
                type="password"
                placeholder="请输入密码"
                show-password
                autocomplete="current-password"
                @keyup.enter="onSubmit"
              />
            </el-form-item>
            <el-button type="primary" class="submit" :loading="loading" @click="onSubmit">
              登 录
            </el-button>
          </el-form>

          <p class="tip">账号权限由角色决定：超级管理员、管理员、只读成员。</p>
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
.page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 32px 20px;
  background:
    radial-gradient(1100px 620px at 18% -10%, rgba(23, 92, 211, 0.28), transparent 62%),
    radial-gradient(900px 560px at 88% 108%, rgba(6, 118, 71, 0.18), transparent 60%),
    #0c1a2b;
}

.shell {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 400px;
  width: min(940px, 100%);
  border: 1px solid rgba(148, 176, 214, 0.16);
  border-radius: 14px;
  overflow: hidden;
  background: var(--gj-surface);
  box-shadow: 0 32px 70px -40px rgba(3, 10, 22, 0.85);
}

/* ---------- 左侧品牌区 ---------- */
.aside {
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  gap: 40px;
  padding: 36px 38px;
  color: var(--gj-nav-text);
  background:
    linear-gradient(rgba(148, 176, 214, 0.07) 1px, transparent 1px) 0 0 / 100% 44px,
    linear-gradient(90deg, rgba(148, 176, 214, 0.07) 1px, transparent 1px) 0 0 / 44px 100%,
    linear-gradient(160deg, #13243a 0%, #0b1727 100%);
}

.aside-top {
  display: flex;
  align-items: center;
  gap: 12px;
}
.logo {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 38px;
  height: 38px;
  border-radius: 10px;
  background: var(--gj-primary);
  color: #fff;
  font-size: 21px;
  font-weight: 700;
  line-height: 1;
}
.wordmark {
  font-size: 18px;
  font-weight: 650;
  letter-spacing: 0.02em;
  color: var(--gj-nav-text-strong);
}

.aside-copy h1 {
  margin: 0;
  font-size: 26px;
  line-height: 1.3;
  font-weight: 650;
  letter-spacing: -0.01em;
  color: var(--gj-nav-text-strong);
}
.lede {
  margin: 10px 0 0;
  font-size: 13px;
  line-height: 1.7;
  color: var(--gj-nav-text);
}

.points {
  margin: 28px 0 0;
  padding: 0;
  list-style: none;
  display: grid;
  gap: 18px;
}
.points li {
  display: flex;
  gap: 12px;
  align-items: flex-start;
}
.dot {
  flex-shrink: 0;
  width: 7px;
  height: 7px;
  margin-top: 6px;
  border-radius: 50%;
  background: #60a5fa;
  box-shadow: 0 0 0 4px rgba(96, 165, 250, 0.14);
}
.points strong {
  display: block;
  font-size: 13px;
  font-weight: 600;
  color: var(--gj-nav-text-strong);
}
.points span:last-child {
  display: block;
  margin-top: 3px;
  font-size: 12px;
  line-height: 1.65;
  color: rgba(201, 214, 230, 0.78);
}

.aside-foot {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12px;
  color: rgba(201, 214, 230, 0.6);
}
.sep {
  width: 3px;
  height: 3px;
  border-radius: 50%;
  background: currentColor;
  opacity: 0.6;
}

/* ---------- 右侧表单区 ---------- */
.panel {
  display: flex;
  align-items: center;
  background: var(--gj-surface);
}
.panel-inner {
  width: 100%;
  padding: 44px 40px;
}

.panel-head h2 {
  margin: 0;
  font-size: 21px;
  font-weight: 650;
  color: var(--gj-text);
}
.panel-head p {
  margin: 6px 0 26px;
  font-size: 13px;
  color: var(--gj-text-muted);
}

.panel :deep(.el-form-item) {
  margin-bottom: 20px;
}
.panel :deep(.el-form-item__label) {
  padding-bottom: 6px;
  font-size: 13px;
  font-weight: 500;
  color: var(--gj-text-regular);
}
.panel :deep(.el-input__wrapper) {
  padding: 4px 12px;
}

.submit {
  width: 100%;
  height: 42px;
  margin-top: 6px;
  font-size: 14px;
  letter-spacing: 0.08em;
}

.tip {
  margin: 22px 0 0;
  padding-top: 16px;
  border-top: 1px solid var(--gj-border-soft);
  font-size: 12px;
  line-height: 1.7;
  color: var(--gj-text-faint);
}

@media (max-width: 860px) {
  .shell {
    grid-template-columns: minmax(0, 1fr);
    width: min(440px, 100%);
  }
  .aside {
    gap: 22px;
    padding: 24px 26px;
  }
  .aside-copy h1 {
    font-size: 21px;
  }
  .lede {
    margin-top: 6px;
  }
  .points,
  .aside-foot {
    display: none;
  }
  .panel-inner {
    padding: 30px 26px 34px;
  }
}

@media (max-width: 420px) {
  .page {
    padding: 0;
    background: #0c1a2b;
  }
  .shell {
    border: 0;
    border-radius: 0;
    min-height: 100vh;
  }
  .panel-inner {
    padding: 32px 22px 40px;
  }
}
</style>
