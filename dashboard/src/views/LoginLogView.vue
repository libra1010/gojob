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
</script>

<template>
  <div>
    <div class="toolbar">
      <h3>登录日志</h3>
      <div class="filters">
        <el-input
          v-model="query.username"
          placeholder="按用户名过滤"
          clearable
          style="width: 200px"
          @keyup.enter="search"
          @clear="search"
        />
        <el-button @click="search">查 询</el-button>
      </div>
    </div>

    <el-table v-loading="loading" :data="rows" border stripe>
      <el-table-column prop="id" label="ID" width="80" />
      <el-table-column prop="username" label="用户名" width="140" />
      <el-table-column label="结果" width="100">
        <template #default="{ row }">
          <el-tag :type="row.success ? 'success' : 'danger'">
            {{ row.success ? '成功' : '失败' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="reason" label="失败原因" width="160" />
      <el-table-column prop="ip" label="来源 IP" width="150" />
      <el-table-column prop="user_agent" label="User-Agent" min-width="220" show-overflow-tooltip />
      <el-table-column label="时间" width="180">
        <template #default="{ row }">{{ formatTime(row.login_time) }}</template>
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
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}
.toolbar h3 {
  margin: 0;
}
.filters {
  display: flex;
  gap: 8px;
}
.pager {
  margin-top: 16px;
  justify-content: flex-end;
}
</style>
