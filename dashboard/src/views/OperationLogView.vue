<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { operationLogs } from '@/api/audit'
import { formatTime } from '@/utils/format'
import type { OperationLog } from '@/api/types'

const loading = ref(false)
const rows = ref<OperationLog[]>([])
const total = ref(0)
const query = reactive({ page: 1, size: 20, username: '', action: '', app_id: '' })

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
  <div>
    <div class="toolbar">
      <h3>操作日志</h3>
      <div class="filters">
        <el-input v-model="query.username" placeholder="用户名" clearable style="width: 140px" @keyup.enter="search" />
        <el-input v-model="query.action" placeholder="动作(如 job/)" clearable style="width: 160px" @keyup.enter="search" />
        <el-input v-model="query.app_id" placeholder="项目" clearable style="width: 140px" @keyup.enter="search" />
        <el-button @click="search">查 询</el-button>
      </div>
    </div>

    <el-table v-loading="loading" :data="rows" border stripe>
      <el-table-column prop="id" label="ID" width="70" />
      <el-table-column prop="username" label="操作人" width="120" />
      <el-table-column prop="role" label="角色" width="120" />
      <el-table-column prop="action" label="动作" width="150" />
      <el-table-column prop="app_id" label="项目" width="120" />
      <el-table-column prop="method" label="方法" width="80" />
      <el-table-column label="结果" width="90">
        <template #default="{ row }">
          <el-tag :type="row.success ? 'success' : 'danger'">
            {{ row.success ? '成功' : row.result_code }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="ip" label="IP" width="130" />
      <el-table-column prop="params" label="参数摘要" min-width="240" show-overflow-tooltip />
      <el-table-column label="时间" width="180">
        <template #default="{ row }">{{ formatTime(row.create_time) }}</template>
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
