<script setup lang="ts">
import { ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { listWorkers, setWorkerAccept } from '@/api/app'
import type { WorkerNodeView } from '@/api/types'
import { sdkTagType } from '@/utils/format'
import { useAuthStore } from '@/stores/auth'

const props = defineProps<{ modelValue: boolean; appId: string }>()
const emit = defineEmits<{ 'update:modelValue': [v: boolean] }>()

const auth = useAuthStore()
const workers = ref<WorkerNodeView[]>([])
const loading = ref(false)
const pending = ref('') // 正在翻开关的那台节点 instance_id，用来只禁用它自己那一行的按钮

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

async function onToggle(w: WorkerNodeView) {
  const accept = !w.accept_tasks
  pending.value = w.instance_id
  try {
    await setWorkerAccept(props.appId, w.instance_id, accept)
    w.accept_tasks = accept
    ElMessage.success(accept ? '已恢复接单' : '已停止接单：在跑的任务照常跑完，新任务不再派给它')
  } catch {
    /* 拦截器已提示 */
  } finally {
    pending.value = ''
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
    size="720px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div class="toolbar">
      <el-button size="small" :loading="loading" @click="load">刷 新</el-button>
      <span class="count">共 {{ workers.length }} 个在线节点</span>
    </div>
    <el-table :data="workers" v-loading="loading" size="small" stripe>
      <el-table-column prop="address" label="地址" min-width="180" show-overflow-tooltip />
      <el-table-column label="SDK 类型" width="150">
        <template #default="{ row }">
          <el-tag :type="sdkTagType(row.sdk_type)" size="small">{{ row.sdk_type }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="接单" width="110">
        <template #default="{ row }">
          <el-tag v-if="row.accept_tasks" type="success" size="small" effect="plain">接单中</el-tag>
          <el-tag v-else type="warning" size="small" effect="plain">已停止接单</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="version" label="版本" width="110" />
      <el-table-column label="CPU 负载" width="90">
        <template #default="{ row }">{{ Number(row.cpu_load).toFixed(2) }}</template>
      </el-table-column>
      <el-table-column label="内存(MB)" width="90">
        <template #default="{ row }">{{ Number(row.memory_used).toFixed(1) }}</template>
      </el-table-column>
      <el-table-column prop="instance_id" label="实例 ID" min-width="200" show-overflow-tooltip />
      <el-table-column v-if="auth.canWrite" label="操作" width="110" fixed="right">
        <template #default="{ row }">
          <el-button
            size="small"
            :type="row.accept_tasks ? 'warning' : 'primary'"
            plain
            :loading="pending === row.instance_id"
            :title="
              row.accept_tasks
                ? '停止派新任务给这台节点（在跑的继续执行并回报）'
                : '恢复派新任务给这台节点'
            "
            @click="onToggle(row)"
          >
            {{ row.accept_tasks ? '停止接单' : '恢复接单' }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty description="该应用暂无在线 Worker 节点" :image-size="80" />
      </template>
    </el-table>
    <p class="tip">
      「停止接单」只影响之后的派单：这台节点上正在跑的任务会继续执行并回报，广播的节点集合按
      触发那一刻的快照算。开关落在数据库里，所以 worker 重启、副本切换都不会把它丢掉。
    </p>
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
.tip {
  margin: 12px 0 0;
  color: #909399;
  font-size: 12px;
  line-height: 1.6;
}
</style>
