<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import Sparkline from '@/components/Sparkline.vue'
import { getNodes } from '@/api/nodes'
import { formatTime, sdkTagType } from '@/utils/format'
import type { LeaseHolder, NodeSeries, NodesResp, ServerNode, WorkerNode } from '@/api/types'

const data = ref<NodesResp>({
  interval_ms: 5000, offline_after_ms: 15000, self: '', lease_ttl_ms: 0, leases: [], servers: [], workers: [],
})
const loading = ref(false)
const auto = ref(true)
const err = ref('')
let timer: number | undefined

// 取不到的指标一律显示"—"，不用 0 冒充"很空闲"
const num = (v: number | undefined, unit = '') =>
  v === undefined || v === null ? '—' : `${v}${unit}`
// 协议里 rss/fd 用 0 表示"本平台取不到"（macOS 没有 /proc、JVM 没有可移植 RSS），
// 这两个 0 不是真实读数，不能显示成 "0 MB / 0"
const maybe = (v: number | undefined, unit = '') => (v ? `${v}${unit}` : '—')
// worker 堆大小直接来自 runtime 字节数，不修一下会显示成 3.1640625 MB
const mb = (v: number | undefined) => (v ? `${Math.round(v * 10) / 10} MB` : '—')
const col = (s: NodeSeries | null | undefined, k: keyof NodeSeries) => (s ? (s[k] as number[]) : [])

const servers = computed(() => data.value.servers)
const workers = computed(() => data.value.workers)
const onlineWorkers = computed(() => workers.value.filter((w) => w.online).length)

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    data.value = await getNodes()
    err.value = ''
  } catch (e) {
    err.value = String(e instanceof Error ? e.message : e)
  } finally {
    loading.value = false
  }
}

function toggle(v: boolean) {
  window.clearInterval(timer)
  if (v) timer = window.setInterval(() => load(true), Math.max(data.value.interval_ms, 2000))
}
function uptime(n: ServerNode) {
  const ms = new Date(n.sample_time).getTime() - new Date(n.boot_time).getTime()
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
// 租约剩余时间：负数说明这一跳还没续约（或持有者已掉线），接管方最多再等一个 TTL。
function left(l: LeaseHolder) {
  if (!l.holder) return '—'
  const ms = new Date(l.lease_expire).getTime() - Date.now()
  return ms > 0 ? `${(ms / 1000).toFixed(1)}s` : '已过期'
}
function silent(ms: number) {
  if (ms < 1000) return '刚刚'
  if (ms < 60000) return `${Math.round(ms / 1000)}s 前`
  return `${Math.round(ms / 60000)}min 前`
}

onMounted(() => {
  load()
  toggle(true)
})
onBeforeUnmount(() => window.clearInterval(timer))
</script>

<template>
  <div class="nodes-page" v-loading="loading">
    <div class="gj-page-head">
      <div class="gj-page-head__main">
        <h3 class="gj-page-title">节点监控</h3>
        <div class="gj-page-sub">
          <span>
            采样间隔 {{ (data.interval_ms / 1000).toFixed(0) }}s，超过
            {{ (data.offline_after_ms / 1000).toFixed(0) }}s 无心跳判离线
          </span>
          <span class="meta-sep" />
          <span v-if="!err">在线 Worker {{ onlineWorkers }} / {{ workers.length }}</span>
          <el-tag v-else type="danger" size="small" effect="plain">{{ err }}</el-tag>
        </div>
      </div>
      <div class="gj-page-actions">
        <el-switch v-model="auto" aria-label="自动刷新" active-text="自动刷新" @change="toggle(auto as boolean)" />
        <el-button size="small" :loading="loading" @click="load()">刷 新</el-button>
      </div>
    </div>

    <el-card shadow="never" class="panel">
      <template #header>
        <div class="panel-head">
          <span class="panel-title">调度中心副本</span>
          <span class="panel-sub">本进程自监控，多副本各自上报</span>
        </div>
      </template>
      <el-table :data="servers" size="small">
        <el-table-column label="节点" min-width="190">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ row.hostname }}</div>
            <div class="sub">{{ row.address || row.public_url }}</div>
          </template>
        </el-table-column>
        <el-table-column label="调度归属" width="150">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ row.online ? `持有 ${row.owned_count} 个项目` : '—' }}</div>
            <div class="sub" :title="(row.owned_apps || []).join(', ')">
              {{ (row.owned_apps || []).slice(0, 3).join('、') || '无' }}
              <span v-if="(row.owned_apps || []).length > 3">…</span>
              <span v-if="row.node_id === data.self">（本机）</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="96">
          <template #default="{ row }: { row: ServerNode }">
            <el-tag :type="row.online ? 'success' : 'danger'" size="small">
              {{ row.online ? '在线' : '离线' }}
            </el-tag>
            <div class="sub">{{ silent(row.silent_ms) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="启动时间" width="150">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ formatTime(row.boot_time) }}</div>
            <div class="sub">已运行 {{ uptime(row) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="CPU" width="150" sortable :sort-by="'cpu_percent'">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ num(row.cpu_percent, '%') }}</div>
            <Sparkline :values="col(row.samples, 'cpu')" :height="22" />
          </template>
        </el-table-column>
        <el-table-column label="内存 RSS / 堆" width="150" sortable :sort-by="'rss_mb'">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ maybe(row.rss_mb, ' MB') }} / {{ num(row.heap_inuse_mb, ' MB') }}</div>
            <Sparkline :values="col(row.samples, 'mem')" :height="22" color="#067647" />
          </template>
        </el-table-column>
        <el-table-column label="GC" width="120">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ row.goroutines }} 协程</div>
            <div class="sub">GC {{ row.gc_count }} 次 / 暂停 {{ num(row.gc_pause_ms, 'ms') }}</div>
          </template>
        </el-table-column>
        <el-table-column label="HTTP 连接" width="150">
          <template #default="{ row }: { row: ServerNode }">
            <div>活跃 {{ row.conns_active }} · 空闲 {{ row.conns_idle }}</div>
            <div class="sub">长轮询在途 {{ row.poll_inflight }}</div>
          </template>
        </el-table-column>
        <el-table-column label="请求 QPS" width="150">
          <template #default="{ row }: { row: ServerNode }">
            <div>{{ num(row.req_per_sec) }}（在途 {{ row.req_inflight }}）</div>
            <Sparkline :values="col(row.samples, 'qps')" :height="22" color="#b54708" />
          </template>
        </el-table-column>
        <template #empty>
          <el-empty
            description="进程启动后第一个采样周期才有数据"
            :image-size="64"
          />
        </template>
      </el-table>
    </el-card>

    <el-card shadow="never" class="panel">
      <template #header>
        <div class="panel-head">
          <span class="panel-title">项目调度归属</span>
          <span class="panel-sub">同一时刻只有一个副本调度同一个项目</span>
        </div>
      </template>
      <el-table :data="data.leases || []" size="small">
        <el-table-column prop="app_id" label="项目" min-width="140">
          <template #default="{ row }: { row: LeaseHolder }">
            <span class="gj-mono">{{ row.app_id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="当前调度副本" min-width="220">
          <template #default="{ row }: { row: LeaseHolder }">
            <span v-if="row.holder" class="gj-mono">{{ row.holder }}</span>
            <span v-else class="sub">尚无副本持有（新项目，等下一跳认领）</span>
            <el-tag v-if="row.holder === data.self" size="small" type="info">本机</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="120">
          <template #default="{ row }: { row: LeaseHolder }">
            <el-tag :type="row.alive ? 'success' : 'warning'" size="small">
              {{ row.alive ? '调度中' : '租约已过期' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="epoch" label="epoch" width="80" />
        <el-table-column label="租约剩余" width="110">
          <template #default="{ row }: { row: LeaseHolder }">{{ left(row) }}</template>
        </el-table-column>
        <template #empty><span class="sub">还没有项目</span></template>
      </el-table>
    </el-card>

    <el-card shadow="never" class="panel">
      <template #header>
        <div class="panel-head">
          <span class="panel-title">Worker 节点</span>
          <span class="panel-sub">
            runtime 指标随心跳上报 · 在线 {{ onlineWorkers }} / 共 {{ workers.length }}
          </span>
        </div>
      </template>
      <el-table :data="workers" size="small">
        <el-table-column prop="app_id" label="项目" width="100">
          <template #default="{ row }: { row: WorkerNode }">
            <span class="gj-mono">{{ row.app_id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="地址" min-width="190">
          <template #default="{ row }: { row: WorkerNode }">
            <div class="gj-mono addr" :title="row.address">{{ row.address }}</div>
            <div class="sub instance-id" :title="row.instance_id">{{ row.instance_id }}</div>
          </template>
        </el-table-column>
        <el-table-column label="SDK" width="132">
          <template #default="{ row }: { row: WorkerNode }">
            <el-tag :type="sdkTagType(row.sdk_type)" size="small">{{ row.sdk_type }}</el-tag>
            <span class="sub"> {{ row.version }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="88">
          <template #default="{ row }: { row: WorkerNode }">
            <el-tag :type="row.online ? 'success' : 'danger'" size="small">
              {{ row.online ? '在线' : '失联' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="CPU 负载 / 占用" width="130">
          <template #default="{ row }: { row: WorkerNode }">
            <div>{{ maybe(Math.round(row.cpu_load * 100) / 100) }} / {{ num(row.cpu_percent, '%') }}</div>
            <div class="sub">{{ row.cpu_processors }} 核可用</div>
          </template>
        </el-table-column>
        <el-table-column label="堆 已用 / 上限 / RSS" width="168">
          <template #default="{ row }: { row: WorkerNode }">
            <div>{{ mb(row.memory_used) }} / {{ row.memory_max ? mb(row.memory_max) : '不限' }}</div>
            <div class="sub">RSS {{ maybe(row.rss_mb, ' MB') }}</div>
          </template>
        </el-table-column>
        <el-table-column label="GC" width="112">
          <template #default="{ row }: { row: WorkerNode }">
            <div>{{ row.gc_count }} 次</div>
            <div class="sub">累计暂停 {{ num(row.gc_pause_ms, 'ms') }}</div>
          </template>
        </el-table-column>
        <el-table-column label="线程 / fd" width="110">
          <template #default="{ row }: { row: WorkerNode }">
            <div>{{ num(row.threads) }} / {{ maybe(row.open_fds) }}</div>
            <div class="sub">Go 协程 / JVM 线程</div>
          </template>
        </el-table-column>
        <el-table-column label="在跑任务" width="84">
          <template #default="{ row }: { row: WorkerNode }">{{ num(row.running_tasks) }}</template>
        </el-table-column>
        <el-table-column prop="tag" label="标签" width="72">
          <template #default="{ row }: { row: WorkerNode }">{{ row.tag || '—' }}</template>
        </el-table-column>
        <el-table-column label="上次心跳" width="156">
          <template #default="{ row }: { row: WorkerNode }">
            <span class="hb">{{ formatTime(row.last_heartbeat) }}</span>
          </template>
        </el-table-column>
        <template #empty><span class="sub">当前没有 worker 节点心跳记录</span></template>
      </el-table>
    </el-card>

    <p class="note">
      说明：RSS / fd / 系统负载需要 Linux 的 <code>/proc</code>，macOS 本地开发显示“—”属正常；
      JVM 侧 RSS 没有可移植取法，恒为“—”。GC 与协程数取自各进程运行时，调度中心为 Go runtime，worker 侧 Go 用 runtime、JVM 用 MXBean。
    </p>
  </div>
</template>

<style scoped>
.meta-sep {
  width: 1px;
  height: 11px;
  background: var(--gj-border);
}
.panel {
  margin-bottom: 16px;
}
.panel :deep(.el-card__body) {
  padding: 6px 16px 10px;
}
.panel-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.panel-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--gj-text);
}
.panel-sub {
  font-size: 12px;
  font-weight: 400;
  color: var(--gj-text-muted);
}
.sub {
  color: var(--gj-text-muted);
  font-size: 12px;
  line-height: 1.55;
}
.addr {
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.hb {
  white-space: nowrap;
}
.instance-id {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.note {
  margin: 4px 0 0;
  color: var(--gj-text-muted);
  font-size: 12px;
  line-height: 1.7;
}
.note code {
  padding: 0 4px;
  border-radius: 3px;
  background: var(--gj-surface-subtle);
  font-family: var(--gj-mono);
}
</style>
