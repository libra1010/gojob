<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { operationLogs } from '@/api/audit'
import { formatTime } from '@/utils/format'
import type { OperationLog } from '@/api/types'

const loading = ref(false)
const rows = ref<OperationLog[]>([])
const total = ref(0)
const query = reactive({ page: 1, size: 20, username: '', action: '', app_id: '' })

const roleLabel: Record<string, string> = {
  SUPER_ADMIN: '超级管理员',
  ADMIN: '管理员',
  READONLY: '只读',
}

async function load() {
  loading.value = true
  try {
    const res = await operationLogs({
      page: query.page,
      size: query.size,
      username: query.username || undefined,
      action: query.action || undefined,
      app_id: query.app_id || undefined,
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
</script>

<template>
  <div class="logs-page">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <h3 class="gj-page-title">操作日志</h3>
        <div class="gj-page-sub">
          <span>共 {{ total }} 条记录 · 记录控制台写操作与结果</span>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-input
          v-model="query.username"
          placeholder="用户名"
          clearable
          class="filter filter-sm"
          @keyup.enter="search"
        />
        <el-input
          v-model="query.action"
          placeholder="动作(如 job/)"
          clearable
          class="filter filter-sm"
          @keyup.enter="search"
        />
        <el-input
          v-model="query.app_id"
          placeholder="项目"
          clearable
          class="filter filter-sm"
          @keyup.enter="search"
        />
        <el-button size="small" type="primary" plain @click="search">查 询</el-button>
      </div>
    </div>

    <el-card shadow="never" class="table-card">
      <el-table v-loading="loading" :data="rows" size="small">
        <el-table-column label="ID" width="72">
          <template #default="{ row }"><span class="gj-mono">#{{ row.id }}</span></template>
        </el-table-column>
        <el-table-column prop="username" label="操作人" width="120">
          <template #default="{ row }"><span class="username">{{ row.username }}</span></template>
        </el-table-column>
        <el-table-column label="角色" width="112">
          <template #default="{ row }"><span class="role">{{ roleLabel[row.role] || row.role }}</span></template>
        </el-table-column>
        <el-table-column label="动作" width="164">
          <template #default="{ row }"><span class="action gj-mono">{{ row.action }}</span></template>
        </el-table-column>
        <el-table-column prop="app_id" label="项目" width="120">
          <template #default="{ row }">
            <span v-if="row.app_id" class="gj-mono">{{ row.app_id }}</span>
            <span v-else class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="方法" width="80">
          <template #default="{ row }"><span class="method">{{ row.method }}</span></template>
        </el-table-column>
        <el-table-column label="结果" width="90">
          <template #default="{ row }">
            <span
              class="gj-pill"
              :class="row.success ? 'gj-pill--ok' : 'gj-pill--bad'"
            >{{ row.success ? '成功' : row.result_code }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="ip" label="IP" width="130">
          <template #default="{ row }"><span class="gj-mono ip">{{ row.ip }}</span></template>
        </el-table-column>
        <el-table-column prop="params" label="参数摘要" min-width="240" show-overflow-tooltip>
          <template #default="{ row }"><span class="ua">{{ row.params || '-' }}</span></template>
        </el-table-column>
        <el-table-column label="时间" width="166">
          <template #default="{ row }"><span class="time">{{ formatTime(row.create_time) }}</span></template>
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
  width: 150px;
}
.filter-sm {
  width: 140px;
}
.username {
  font-weight: 500;
  color: var(--gj-text);
}
.role {
  font-size: 12px;
  color: var(--gj-text-regular);
}
.action,
.method {
  font-size: 12px;
  color: var(--gj-text-regular);
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
@media (max-width: 1100px) {
  .gj-page-actions {
    flex-wrap: wrap;
  }
}
</style>
