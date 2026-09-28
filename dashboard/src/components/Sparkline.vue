<script setup lang="ts">
import { computed } from 'vue'

// 无依赖的迷你折线：节点监控页要同时画十几个指标，引图表库不值得。
const props = withDefaults(
  defineProps<{
    values?: number[]
    width?: number
    height?: number
    color?: string
  }>(),
  { values: () => [], width: 130, height: 30, color: '#409eff' },
)

const points = computed(() => {
  const vs = props.values
  if (!vs || vs.length < 2) return ''
  const min = Math.min(...vs)
  const max = Math.max(...vs)
  const step = props.width / (vs.length - 1)
  if (max === min) return vs.map((_, i) => `${(i * step).toFixed(1)},${(props.height / 2).toFixed(1)}`).join(' ')
  const span = max - min
  return vs.map((v, i) => `${(i * step).toFixed(1)},${(props.height - ((v - min) / span) * props.height).toFixed(1)}`).join(' ')
})
</script>

<template>
  <svg v-if="points" :width="width" :height="height" class="spark" role="img">
    <polyline :points="points" fill="none" :stroke="color" stroke-width="1.5" />
  </svg>
  <span v-else class="spark-empty">—</span>
</template>

<style scoped>
.spark {
  display: block;
  vertical-align: middle;
}
.spark-empty {
  color: var(--el-text-color-secondary);
}
</style>
