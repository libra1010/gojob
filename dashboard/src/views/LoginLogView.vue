<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { loginLogs } from '@/api/audit'
import { formatTime } from '@/utils/format'
import type { LoginLog } from '@/api/types'

const loading = ref(false)
const rows = ref<LoginLog[]>([])
const total = ref(0)
const query = reactive({ page: 1, size: 20, username: '' })

async function load() {
  loading.value = true
  try {
    const res = await loginLogs({
      page: query.page,
      size: query.size,
      username: query.username || undefined,
    })
    rows.value = res.rows
    total.value = res.total
  } finally {
    loading.value = false
  }
}
function search() {
  query.page = 1
  load()
}
onMounted(load)

/** 后端返回的失败原因是英文串，控制台统一显示中文 */
function reasonLabel(reason?: string) {
  switch (reason) {
    case 'wrong password':
      return '密码错误'
    case 'account disabled':
      return '账号已禁用'
    case 'wrong username or password':
      return '用户名或密码错误'
    case 'account disabled or removed':
      return '账号已禁用或已删除'
    default:
      return reason || '-'
  }
}
</script>

<template>
  <div class="logs-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <h3 class="gj-page-title">登录日志</h3>
        <div class="gj-page-sub">
          <span>共 {{ total }} 条记录 · 记录每次控制台登录尝试</span>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-input
          v-model="query.username"
          placeholder="按用户名过滤"
          clearable
          class="filter"
          @keyup.enter="search"
          @clear="search"
        />
        <el-button size="small" type="primary" plain @click="search">查 询</el-button>
      </div>
    </div>

    <el-card shadow="never" class="table-card">
      <el-table v-loading="loading" :data="rows" size="small">
        <el-table-column label="ID" width="76">
          <template #default="{ row }"><span class="gj-mono">#{{ row.id }}</span></template>
        </el-table-column>
        <el-table-column prop="username" label="用户名" width="140">
          <template #default="{ row }"><span class="username">{{ row.username }}</span></template>
        </el-table-column>
        <el-table-column label="结果" width="92">
          <template #default="{ row }">
            <span
              class="gj-pill"
              :class="row.success ? 'gj-pill--ok' : 'gj-pill--bad'"
            >{{ row.success ? '成功' : '失败' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="失败原因" width="170">
          <template #default="{ row }">
            <span :class="{ muted: !row.reason }">{{ reasonLabel(row.reason) }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="ip" label="来源 IP" width="150">
          <template #default="{ row }"><span class="gj-mono ip">{{ row.ip }}</span></template>
        </el-table-column>
        <el-table-column prop="user_agent" label="User-Agent" min-width="220" show-overflow-tooltip>
          <template #default="{ row }"><span class="ua">{{ row.user_agent || '-' }}</span></template>
        </el-table-column>
        <el-table-column label="时间" width="166">
          <template #default="{ row }"><span class="time">{{ formatTime(row.login_time) }}</span></template>
        </el-table-column>
      </el-table>

      <el-pagination
        class="pager"
        layout="total, prev, pager, next, sizes"
        :total="total"
        v-model:current-page="query.page"
        v-model:page-size="query.size"
        :page-sizes="[20, 50, 100]"
        @current-change="load"
        @size-change="search"
      />
    </el-card>
  </div>
</template>

<style scoped>
.table-card :deep(.el-card__body) {
  padding: 6px 16px 10px;
}
.filter {
  width: 220px;
}
.username {
  font-weight: 500;
  color: var(--gj-text);
}
.ip {
  color: var(--gj-text-regular);
  font-size: 12px;
}
.ua {
  font-size: 12px;
  color: var(--gj-text-muted);
}
.time {
  font-size: 12px;
  color: var(--gj-text-muted);
  font-variant-numeric: tabular-nums;
}
.muted {
  color: var(--gj-text-faint);
}
.pager {
  margin-top: 12px;
  justify-content: flex-end;
}
</style>
