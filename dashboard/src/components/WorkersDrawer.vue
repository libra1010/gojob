<script setup lang="ts">
import { ref, watch } from 'vue'
import { listWorkers } from '@/api/app'
import type { WorkerInfo } from '@/api/types'
import { sdkTagType } from '@/utils/format'

const props = defineProps<{ modelValue: boolean; appId: string }>()
const emit = defineEmits<{ 'update:modelValue': [v: boolean] }>()

const workers = ref<WorkerInfo[]>([])
const loading = ref(false)

async function load() {
  if (!props.appId) return
  loading.value = true
  try {
    workers.value = await listWorkers(props.appId)
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}

watch(
  () => props.modelValue,
  (v) => {
    if (v) load()
  },
)
</script>

<template>
  <el-drawer
    :model-value="modelValue"
    :title="`在线机器 - ${appId}`"
    size="560px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div class="toolbar">
      <el-button size="small" :loading="loading" @click="load">刷 新</el-button>
      <span class="count">共 {{ workers.length }} 个在线节点</span>
    </div>
    <el-table :data="workers" v-loading="loading" size="small" stripe>
      <el-table-column prop="address" label="地址" min-width="200" show-overflow-tooltip />
      <el-table-column label="SDK 类型" width="150">
        <template #default="{ row }">
          <el-tag :type="sdkTagType(row.sdk_type)" size="small">{{ row.sdk_type }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="version" label="版本" width="120" />
      <el-table-column label="CPU 负载" width="100">
        <template #default="{ row }">{{ Number(row.cpu_load).toFixed(2) }}</template>
      </el-table-column>
      <el-table-column label="内存(MB)" width="100">
        <template #default="{ row }">{{ Number(row.memory_used).toFixed(1) }}</template>
      </el-table-column>
      <el-table-column prop="instance_id" label="实例 ID" min-width="220" show-overflow-tooltip />
      <template #empty>
        <el-empty description="该应用暂无在线 Worker 节点" :image-size="80" />
      </template>
    </el-table>
  </el-drawer>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}
.count {
  color: #909399;
  font-size: 13px;
}
</style>
