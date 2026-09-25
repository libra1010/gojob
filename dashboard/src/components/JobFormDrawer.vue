<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { createJob, modifyJob, previewTime } from '@/api/job'
import type { CreateJobReq, DailyIntervalExpress, JobInfo, ModifyJobReq } from '@/api/types'

const props = defineProps<{
  modelValue: boolean
  appId: string
  job: JobInfo | null
  /** 复制新建：把这份配置的字段灌进「新建」表单（不落库，由用户改名后保存） */
  copyFrom?: JobInfo | null
  /** 同项目全部任务，用于「依赖触发」选上游 */
  jobs: JobInfo[]
}>()
const emit = defineEmits<{ 'update:modelValue': [v: boolean]; saved: [] }>()

const editing = computed(() => props.job !== null)
const copying = computed(() => !editing.value && props.copyFrom !== null && props.copyFrom !== undefined)

const formRef = ref<FormInstance>()
const submitting = ref(false)

/** 表单模型与 CreateJobReq 字段（去掉 app_id）一一对应，编辑时全量下发 */
function defaultForm(): Omit<CreateJobReq, 'app_id'> {
  return {
    job_name: '',
    job_description: '',
    processor: '',
    processor_type: 'BUILT_IN',
    execute_type: 'STANDALONE',
    time_expression_type: 'CRON',
    time_expression: '',
    job_params: '',
    instance_params: '',
    max_retry_times: 3,
    instance_retry_num: 3,
    concurrency: 5,
    instance_num_limit: 3,
    skip_when_running: false,
    designated_workers: '',
    max_worker_count: 0,
    dispatch_strategy: 1,
    dispatch_strategy_config: '',
    upstream_job_id: 0,
    log_level: 'INFO',
  }
}
const form = reactive(defaultForm())

function fillFromJob(j: JobInfo) {
  Object.assign(form, {
    job_name: j.job_name,
    job_description: j.job_description ?? '',
    processor: j.processor,
    processor_type: j.processor_type || 'BUILT_IN',
    execute_type: j.execute_type || 'STANDALONE',
    time_expression_type: j.time_expression_type || 'CRON',
    time_expression: j.time_expression,
    job_params: j.job_params ?? '',
    instance_params: j.instance_params ?? '',
    max_retry_times: j.max_retry_times,
    instance_retry_num: j.instance_retry_num,
    concurrency: j.concurrency,
    instance_num_limit: j.instance_num_limit,
    skip_when_running: j.skip_when_running,
    designated_workers: j.designated_workers ?? '',
    max_worker_count: j.max_worker_count,
    dispatch_strategy: j.dispatch_strategy || 1,
    dispatch_strategy_config: j.dispatch_strategy_config ?? '',
    upstream_job_id: j.upstream_job_id ?? 0,
    log_level: j.log_level || 'INFO',
  })
  if (form.time_expression_type === 'DAILY_TIME_INTERVAL') exprToDaily(j.time_expression)
}

watch(
  () => props.modelValue,
  (v) => {
    if (!v) return
    Object.assign(form, defaultForm())
    const src = props.job ?? props.copyFrom
    if (src) {
      fillFromJob(src)
      // 复制新建：任务名加 _copy 后缀，避免与源任务同名（同名不会报错，但两份同名任务很难辨认）
      if (!props.job) form.job_name = `${src.job_name}_copy`
    }
    advanced.value = []
    formRef.value?.clearValidate()
  },
)

const rules: FormRules = {
  job_name: [{ required: true, message: '请输入任务名称', trigger: 'blur' }],
  processor: [{ required: true, message: '请输入处理器类名', trigger: 'blur' }],
  time_expression: [
    {
      validator: (_r, value: string, cb: (e?: Error) => void) => {
        // API/依赖触发不由时间表达式驱动
        if (form.time_expression_type === 'API' || form.time_expression_type === 'DEPENDENCY') return cb()
        if (form.time_expression_type === 'DAILY_TIME_INTERVAL') {
          const e = validateDaily()
          return e ? cb(new Error(e)) : cb()
        }
        if (!value || !value.trim()) return cb(new Error('请输入时间表达式'))
        if (form.time_expression_type === 'FIXED_RATE' || form.time_expression_type === 'FIXED_DELAY') {
          const n = Number(value.trim())
          if (!/^\d+$/.test(value.trim()) || n <= 0) return cb(new Error('请填写正整数周期（毫秒）'))
          if (n > 120000) return cb(new Error('周期不能超过 120000 毫秒'))
        }
        cb()
      },
      trigger: 'blur',
    },
  ],
  upstream_job_id: [
    {
      validator: (_r, _v: unknown, cb: (e?: Error) => void) => {
        if (form.time_expression_type !== 'DEPENDENCY') return cb()
        return form.upstream_job_id > 0
          ? cb()
          : cb(new Error('依赖触发需要选择一个上游任务'))
      },
      trigger: 'change',
    },
  ],
}

const TIME_OF_DAY_RE = /^(\d{1,2}):(\d{1,2})(:(\d{1,2}))?$/

function timeOfDayError(v: string): string | null {
  const m = TIME_OF_DAY_RE.exec(v.trim())
  if (!m) return '时间格式需为 HH:mm:ss 或 HH:mm'
  const [h, mi, s] = [Number(m[1]), Number(m[2]), Number(m[4] ?? 0)]
  if (h > 23 || mi > 59 || s > 59) return '时间超出范围'
  return null
}

function todSeconds(v: string): number {
  const m = TIME_OF_DAY_RE.exec(v.trim())
  if (!m) return 0
  return Number(m[1]) * 3600 + Number(m[2]) * 60 + Number(m[4] ?? 0)
}

/** 每日固定间隔的字段校验，返回错误文案（合法则 null） */
function validateDaily(): string | null {
  if (!Number.isInteger(daily.interval) || daily.interval <= 0) return '触发间隔需为正整数'
  const ms = daily.interval * DAILY_UNIT_MS[daily.intervalUnit]
  if (ms <= 0) return '换算后的间隔必须大于 0 毫秒'
  const s = timeOfDayError(daily.startTimeOfDay)
  if (s) return `时间窗开始：${s}`
  const e = timeOfDayError(daily.endTimeOfDay)
  if (e) return `时间窗结束：${e}`
  if (todSeconds(daily.endTimeOfDay) < todSeconds(daily.startTimeOfDay)) return '时间窗结束不能早于开始'
  if (daily.daysOfWeek.length > 0 && daily.daysOfWeek.length < 7) {
    if (daily.daysOfWeek.some((d) => !Number.isInteger(d) || d < 1 || d > 7)) {
      return '生效星期需在 周一~周日 之间'
    }
  }
  return null
}

/** 时间类型：对齐 PowerJob TimeExpressionType */
const timeTypes = [
  { value: 'CRON', label: 'CRON', hint: '支持 5/6 段 cron，如 0 0/5 * * * ?（每 5 分钟）' },
  {
    value: 'FIXED_RATE',
    label: '固定频率（毫秒）',
    hint: '填周期毫秒数，如 1500 表示每 1.5 秒一次；以上次计划时间为基准，不累积漂移。上限 120000ms',
  },
  {
    value: 'FIXED_DELAY',
    label: '固定延迟（毫秒）',
    hint: '填周期毫秒数，从本次实际触发时刻再等一个周期；上限 120000ms',
  },
  {
    value: 'DAILY_TIME_INTERVAL',
    label: '每日固定间隔',
    hint: '每天只在指定时间窗内、按固定间隔触发（如工作日 09:00~18:00 每 30 分钟）',
  },
  {
    value: 'DEPENDENCY',
    label: '依赖触发（上游任务成功）',
    hint: '不按时间调度：选定一个上游任务，它的一次执行成功后自动触发本任务运行一次（gojob 扩展，PowerJob 用工作流表达依赖）',
  },
  { value: 'API', label: 'API', hint: '不自动调度，仅通过「触发」按钮或 OpenAPI 运行' },
]
const isFrequency = computed(
  () => form.time_expression_type === 'FIXED_RATE' || form.time_expression_type === 'FIXED_DELAY',
)
const isDaily = computed(() => form.time_expression_type === 'DAILY_TIME_INTERVAL')
const isDependency = computed(() => form.time_expression_type === 'DEPENDENCY')
const timeHint = computed(
  () => timeTypes.find((t) => t.value === form.time_expression_type)?.hint ?? '',
)
const exprPlaceholder = computed(() =>
  isFrequency.value
    ? '周期毫秒数，如 1500'
    : form.time_expression_type === 'API'
      ? '（手动触发，留空）'
      : isDaily.value
        ? '（由下方时间窗与间隔自动生成）'
        : isDependency.value
          ? '（由上游任务成功触发，无需表达式）'
          : '如 0 0/5 * * * ?',
)

// ---------- 每日固定间隔：结构化输入，序列化为 PowerJob DailyTimeIntervalExpress JSON ----------
const dayOptions = [
  { v: 1, label: '周一' },
  { v: 2, label: '周二' },
  { v: 3, label: '周三' },
  { v: 4, label: '周四' },
  { v: 5, label: '周五' },
  { v: 6, label: '周六' },
  { v: 7, label: '周日' },
]
const dailyUnits = ['MILLISECONDS', 'SECONDS', 'MINUTES', 'HOURS'] as const
const DAILY_UNIT_MS: Record<(typeof dailyUnits)[number], number> = {
  MILLISECONDS: 1,
  SECONDS: 1000,
  MINUTES: 60_000,
  HOURS: 3_600_000,
}
const dailyUnitLabels: Record<(typeof dailyUnits)[number], string> = {
  MILLISECONDS: '毫秒',
  SECONDS: '秒',
  MINUTES: '分钟',
  HOURS: '小时',
}
const daily = reactive({
  interval: 30,
  intervalUnit: 'MINUTES' as (typeof dailyUnits)[number],
  startTimeOfDay: '09:00:00',
  endTimeOfDay: '18:00:00',
  daysOfWeek: [] as number[],
})

function dailyToExpr(): string {
  return JSON.stringify({
    interval: Number(daily.interval),
    intervalUnit: daily.intervalUnit,
    startTimeOfDay: daily.startTimeOfDay,
    endTimeOfDay: daily.endTimeOfDay,
    daysOfWeek: daily.daysOfWeek.length === 7 || daily.daysOfWeek.length === 0
      ? undefined
      : [...daily.daysOfWeek].sort((a, b) => a - b),
  })
}

/** 编辑回填：解析已存的 JSON 表达式；非法则保留默认值让用户重填 */
function exprToDaily(expr: string) {
  if (!expr?.trim()) return
  try {
    const o = JSON.parse(expr) as Partial<DailyIntervalExpress>
    if (typeof o.interval === 'number') daily.interval = o.interval
    if (o.intervalUnit && (dailyUnits as readonly string[]).includes(o.intervalUnit)) {
      daily.intervalUnit = o.intervalUnit
    }
    if (o.startTimeOfDay) daily.startTimeOfDay = o.startTimeOfDay
    if (o.endTimeOfDay) daily.endTimeOfDay = o.endTimeOfDay
    daily.daysOfWeek = Array.isArray(o.daysOfWeek) ? [...o.daysOfWeek] : []
  } catch {
    /* 保留默认 */
  }
}

// 结构化输入实时序列化为 time_expression（后端只认这一个字段）
watch(
  () => [
    form.time_expression_type,
    daily.interval,
    daily.intervalUnit,
    daily.startTimeOfDay,
    daily.endTimeOfDay,
    daily.daysOfWeek.join(','),
  ] as const,
  () => {
    if (form.time_expression_type === 'DAILY_TIME_INTERVAL') form.time_expression = dailyToExpr()
    // 依赖触发没有表达式，切到这个类型就清掉，避免留下"看着像 cron 其实不生效"的值
    else if (form.time_expression_type === 'DEPENDENCY') form.time_expression = ''
  },
)

// 预览最近触发时间
const previewVisible = ref(false)
const previewing = ref(false)
const previewTimes = ref<string[]>([])
async function onPreview() {
  if (form.time_expression_type === 'API') {
    previewTimes.value = []
    previewVisible.value = true
    return
  }
  if (isDaily.value) {
    const e = validateDaily()
    if (e) {
      ElMessage.warning(e)
      return
    }
    form.time_expression = dailyToExpr()
  }
  previewing.value = true
  try {
    const res = await previewTime(form.time_expression_type, form.time_expression, 20)
    previewTimes.value = res.preview
    previewVisible.value = true
  } catch {
    /* 拦截器已提示（表达式非法） */
  } finally {
    previewing.value = false
  }
}

/** 执行类型：与 PowerJob ExecuteType 同值同名；WORKFLOW 需要工作流引擎，gojob 未实现故禁用 */
const executeTypes = [
  { value: 'STANDALONE', label: '单机（STANDALONE）', disabled: false },
  { value: 'BROADCAST', label: '广播（BROADCAST）', disabled: false },
  { value: 'MAP', label: 'Map（MAP）', disabled: false },
  { value: 'MAP_REDUCE', label: 'MapReduce（MAP_REDUCE）', disabled: false },
  { value: 'WORKFLOW', label: '工作流（WORKFLOW）· 后续版本支持', disabled: true },
]
const executeHints: Record<string, string> = {
  STANDALONE: '一次触发只在一个执行节点上跑一个任务。',
  BROADCAST: '一次触发在每个在线执行节点上各跑一份，全部成功才算成功；节点宕机时那一份直接判失败，不换机重跑（避免同一份活干两遍）。',
  MAP: '处理器先跑根任务，运行期用 ctx.Map 派生任意多个子任务散到集群执行；任一子任务最终失败即本次执行失败。',
  MAP_REDUCE: '在 Map 之上多一步 reduce：子任务全部收尾后执行 reduce，由它的返回值决定本次执行的成败与结果，因此可以在这里容忍部分失败。',
  WORKFLOW: 'gojob 未实现工作流引擎；任务前后置关系请把「时间类型」选成依赖触发（上游任务成功后自动拉起）。',
}
/** 需要处理器实现 map/reduce 接口的类型 */
const mapLike = (t: string) => t === 'MAP' || t === 'MAP_REDUCE'
/** 处理器类型：与 PowerJob 的 ProcessorType 同值同名（SHELL/PYTHON 在其 5.x 已废弃，见下） */
const processorTypes = [
  { value: 'BUILT_IN', label: '内建（BUILT_IN）' },
  { value: 'EXTERNAL', label: '外置（动态加载）（EXTERNAL）' },
]

/**
 * 处理器信息输入框的占位提示，逐字照搬 PowerJob 控制台 5.1.2 的 verifyPlaceholder 与其 i18n 文案。
 * PowerJob 那份还留着 SHELL/PYTHON 两个分支（下拉里已注释掉，永远选不到）；我们不必复刻死分支：
 * 导入到这两种类型时 Server 会明确改写（SHELL → 官方脚本处理器）或拒绝（PYTHON）。
 */
function processorPlaceholder(type: string): string {
  switch (type) {
    case 'EXTERNAL':
      return '容器ID#全限定类名，eg：1#tech.powerjob.HelloWordProcessor'
    default:
      return '全限定类名，eg：tech.powerjob.HelloWordProcessor'
  }
}
const logLevels = ['DEBUG', 'INFO', 'WARN', 'ERROR', 'OFF']

/** 分发策略：1/2/11 与 PowerJob DispatchStrategy 同值，21 为 gojob 扩展 */
const strategies = [
  {
    value: 1,
    label: '健康优先（HEALTH_FIRST）',
    hint: '按 (内存上限−已用)×2 + (可用核数−CPU负载) 取分数最高的在线节点，同 PowerJob 打分公式',
  },
  { value: 2, label: '随机（RANDOM）', hint: '候选节点中随机选一台' },
  {
    value: 11,
    label: '指定机器（SPECIFY）',
    hint: '按匹配规则过滤后随机选一台，落空则降级随机。语法：tagEquals:灰度 / tagIn:灰度 / 逗号分隔 ip:port',
  },
  {
    value: 21,
    label: '轮询（ROUND_ROBIN）· gojob 扩展',
    hint: '按任务游标依次分发，实现 A→B→A 严格轮流执行；PowerJob 无此策略',
  },
]
const isSpecify = computed(() => form.dispatch_strategy === 11)
const strategyHint = computed(
  () => strategies.find((s) => s.value === form.dispatch_strategy)?.hint ?? '',
)

/**
 * 可作为上游的候选：同项目、非自身、且不是自己的下游。
 * 后一条是防御性的——若把下游选成上游就成环了，后端也会拒（保存期做链上回溯检测）。
 */
const upstreamOptions = computed(() => {
  const selfId = props.job?.id ?? 0
  const children = new Map<number, number[]>()
  for (const j of props.jobs) {
    if (!j.upstream_job_id) continue
    const list = children.get(j.upstream_job_id) ?? []
    list.push(j.id)
    children.set(j.upstream_job_id, list)
  }
  const descendants = new Set<number>([selfId])
  const queue = [selfId]
  while (queue.length) {
    for (const child of children.get(queue.shift()!) ?? []) {
      if (descendants.has(child)) continue
      descendants.add(child)
      queue.push(child)
    }
  }
  return props.jobs.filter((j) => j.app_id === props.appId && !descendants.has(j.id))
})

const advanced = ref<string[]>([])

async function onSubmit() {
  if (isDaily.value) form.time_expression = dailyToExpr()
  // 依赖关系只在「依赖触发」这一种调度方式下存在，切换类型时一并清掉
  if (!isDependency.value) form.upstream_job_id = 0
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return
  submitting.value = true
  try {
    if (props.job) {
      const req: ModifyJobReq = {
        job_id: props.job.id,
        job_name: form.job_name,
        job_description: form.job_description,
        processor: form.processor,
        processor_type: form.processor_type,
        execute_type: form.execute_type,
        time_expression_type: form.time_expression_type,
        time_expression: form.time_expression,
        job_params: form.job_params,
        instance_params: form.instance_params,
        max_retry_times: form.max_retry_times,
        instance_retry_num: form.instance_retry_num,
        concurrency: form.concurrency,
        instance_num_limit: form.instance_num_limit,
        skip_when_running: form.skip_when_running,
        designated_workers: form.designated_workers,
        max_worker_count: form.max_worker_count,
        dispatch_strategy: form.dispatch_strategy,
        dispatch_strategy_config: form.dispatch_strategy_config,
        upstream_job_id: form.upstream_job_id,
        log_level: form.log_level,
      }
      await modifyJob(req)
      ElMessage.success('任务已更新')
    } else {
      const req: CreateJobReq = {
        app_id: props.appId,
        job_name: form.job_name,
        job_description: form.job_description,
        processor: form.processor,
        processor_type: form.processor_type,
        execute_type: form.execute_type,
        time_expression_type: form.time_expression_type,
        time_expression: form.time_expression,
        job_params: form.job_params,
        instance_params: form.instance_params,
        max_retry_times: form.max_retry_times,
        instance_retry_num: form.instance_retry_num,
        concurrency: form.concurrency,
        instance_num_limit: form.instance_num_limit,
        skip_when_running: form.skip_when_running,
        designated_workers: form.designated_workers,
        max_worker_count: form.max_worker_count,
        dispatch_strategy: form.dispatch_strategy,
        dispatch_strategy_config: form.dispatch_strategy_config,
        upstream_job_id: form.upstream_job_id,
        log_level: form.log_level,
      }
      await createJob(req)
      ElMessage.success('任务已创建')
    }
    emit('update:modelValue', false)
    emit('saved')
  } catch {
    /* 拦截器已提示（如 cron 非法） */
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <el-drawer
    :model-value="modelValue"
    :title="editing ? '编辑任务' : copying ? '复制为新任务' : '新建任务'"
    size="580px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
      <div class="group-title">基础信息</div>
      <el-form-item label="任务名称" prop="job_name">
        <el-input v-model="form.job_name" placeholder="如 历史数据归档" />
      </el-form-item>
      <el-form-item label="任务描述" prop="job_description">
        <el-input v-model="form.job_description" placeholder="选填，说明该任务的用途" />
      </el-form-item>
      <el-row :gutter="12">
        <el-col :span="12">
          <el-form-item label="执行类型" prop="execute_type">
            <el-select v-model="form.execute_type" class="full">
              <el-option
                v-for="t in executeTypes"
                :key="t.value"
                :label="t.label"
                :value="t.value"
                :disabled="t.disabled"
              />
            </el-select>
            <div class="form-hint">{{ executeHints[form.execute_type] }}</div>
            <div v-if="mapLike(form.execute_type)" class="form-hint">
              需要执行节点实现对应的接口：Go SDK 是 ctx.Map / worker.Reducer，Java SDK 是
              MapProcessor / MapReduceProcessor。没实现却选了这个类型，任务会在执行期明确报错。
            </div>
          </el-form-item>
        </el-col>
        <el-col :span="12">
          <el-form-item label="处理器类型" prop="processor_type">
            <el-select v-model="form.processor_type" class="full">
              <el-option
                v-for="t in processorTypes"
                :key="t.value"
                :label="t.label"
                :value="t.value"
              />
            </el-select>
          </el-form-item>
        </el-col>
      </el-row>
      <el-form-item label="处理器类名" prop="processor">
        <el-input v-model="form.processor" :placeholder="processorPlaceholder(form.processor_type)" />
        <div class="hint">
          gojob 的 BUILT_IN 除类全名外还认 worker 的注册短名（如 hello）。
          <template v-if="form.processor_type === 'EXTERNAL'">
            gojob 的 worker 不做 JAR 动态加载，选此项任务会直接被 worker 判失败。
          </template>
          <template v-else>
            跑 Shell 脚本：处理器类名填
            <code>tech.powerjob.official.processors.impl.script.ShellProcessor</code>
            （或短名 shell），脚本正文写在“任务参数”里，由 gojob-agent 执行。
          </template>
        </div>
      </el-form-item>

      <div class="group-title">定时信息</div>
      <el-row :gutter="12">
        <el-col :span="12">
          <el-form-item label="时间类型" prop="time_expression_type">
            <el-select v-model="form.time_expression_type" class="full">
              <el-option v-for="t in timeTypes" :key="t.value" :label="t.label" :value="t.value" />
            </el-select>
          </el-form-item>
        </el-col>
        <el-col :span="12">
          <el-form-item label="时间表达式" prop="time_expression">
            <div class="expr-row">
              <el-input
                v-model="form.time_expression"
                :disabled="form.time_expression_type === 'API' || isDaily || isDependency"
                :placeholder="exprPlaceholder"
              />
              <el-button
                v-if="form.time_expression_type !== 'API' && !isDependency"
                :loading="previewing"
                @click="onPreview"
                >预 览</el-button
              >
            </div>
            <div class="hint">{{ timeHint }}</div>
          </el-form-item>
        </el-col>
      </el-row>
      <el-form-item v-if="isDependency" label="上游任务" prop="upstream_job_id">
        <el-select
          v-model="form.upstream_job_id"
          class="full"
          filterable
          placeholder="选择本任务等待哪个任务执行成功后触发"
        >
          <el-option
            v-for="j in upstreamOptions"
            :key="j.id"
            :value="j.id"
            :label="`#${j.id} ${j.job_name}`"
          />
        </el-select>
        <div class="hint">
          上游任务的一次执行「成功」结束，就触发本任务执行一次；上游失败或被停止不会触发。
          上游需与本任务同属本项目，且不能与本任务成环。
        </div>
      </el-form-item>
      <div v-if="isDaily" class="daily-box">
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="触发间隔">
              <el-input-number v-model="daily.interval" :min="1" :max="86400000" class="full" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="间隔单位">
              <el-select v-model="daily.intervalUnit" class="full">
                <el-option
                  v-for="u in dailyUnits"
                  :key="u"
                  :label="`${dailyUnitLabels[u]}（${u}）`"
                  :value="u"
                />
              </el-select>
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="时间窗开始">
              <el-input v-model="daily.startTimeOfDay" placeholder="HH:mm:ss，如 09:00:00" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="时间窗结束">
              <el-input v-model="daily.endTimeOfDay" placeholder="HH:mm:ss，如 18:00:00" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="生效星期（不选=每天）">
          <el-checkbox-group v-model="daily.daysOfWeek">
            <el-checkbox v-for="d in dayOptions" :key="d.v" :value="d.v">{{ d.label }}</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
        <div class="hint">只在所选星期的「开始~结束」时间窗内按间隔触发；窗口外的时刻顺延到下一个有效日的窗口起点。</div>
      </div>
      <el-form-item label="任务参数" prop="job_params">
        <el-input
          v-model="form.job_params"
          type="textarea"
          :rows="3"
          placeholder="发给处理器的静态参数：业务处理器一般约定 JSON，如 {&quot;days&quot;:7}；官方脚本处理器在这里填脚本正文"
        />
        <div class="hint">
          后端不校验格式（与 PowerJob 一致），由处理器自己解释这一串文本。
          触发时传的实例参数优先于任务参数，两者都为空时脚本处理器会直接判失败。
        </div>
      </el-form-item>
      <el-form-item label="实例参数" prop="instance_params">
        <el-input
          v-model="form.instance_params"
          type="textarea"
          :rows="3"
          placeholder="每次触发的默认运行时参数（instanceParams），可被手动触发时覆盖"
        />
      </el-form-item>

      <el-collapse v-model="advanced" class="advanced">
        <el-collapse-item title="高级配置" name="advanced">
          <el-row :gutter="12">
            <el-col :span="12">
              <el-form-item label="最大重试次数" prop="max_retry_times">
                <el-input-number v-model="form.max_retry_times" :min="0" :max="100" class="full" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="实例重试次数" prop="instance_retry_num">
                <el-input-number
                  v-model="form.instance_retry_num"
                  :min="0"
                  :max="100"
                  class="full"
                />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="并发度" prop="concurrency">
                <el-input-number v-model="form.concurrency" :min="1" :max="1000" class="full" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="实例数上限" prop="instance_num_limit">
                <el-input-number
                  v-model="form.instance_num_limit"
                  :min="0"
                  :max="1000"
                  class="full"
                />
                <div class="hint">0 表示不限制</div>
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="最大 Worker 数" prop="max_worker_count">
                <el-input-number
                  v-model="form.max_worker_count"
                  :min="0"
                  :max="1000"
                  class="full"
                />
                <div class="hint">0 表示不限制</div>
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="日志级别" prop="log_level">
                <el-select v-model="form.log_level" class="full">
                  <el-option v-for="l in logLevels" :key="l" :label="l" :value="l" />
                </el-select>
              </el-form-item>
            </el-col>
          </el-row>
          <el-form-item label="分发策略" prop="dispatch_strategy">
            <el-select v-model="form.dispatch_strategy" class="full">
              <el-option v-for="s in strategies" :key="s.value" :label="s.label" :value="s.value" />
            </el-select>
            <div class="hint">{{ strategyHint }}</div>
          </el-form-item>
          <el-form-item v-if="isSpecify" label="匹配规则" prop="dispatch_strategy_config">
            <el-input
              v-model="form.dispatch_strategy_config"
              placeholder="如 tagEquals:gray 或 127.0.0.1:7799,127.0.0.1:7800"
            />
          </el-form-item>
          <el-form-item label="指定机器" prop="designated_workers">
            <el-input
              v-model="form.designated_workers"
              placeholder="逗号分隔的 worker 地址，如 127.0.0.1:7700,127.0.0.1:7701"
            />
            <div class="hint">留空表示由调度器在在线机器中自动选择</div>
          </el-form-item>
          <el-form-item label="调度策略" prop="skip_when_running">
            <el-switch v-model="form.skip_when_running" active-text="任务执行中则不发起新任务" />
          </el-form-item>
        </el-collapse-item>
      </el-collapse>
    </el-form>
    <el-dialog
      v-model="previewVisible"
      title="最近触发时间预览"
      width="360px"
      append-to-body
    >
      <el-alert
        v-if="form.time_expression_type === 'API'"
        title="该任务为手动触发（API）类型，无自动调度时间"
        type="info"
        :closable="false"
      />
      <el-table v-else :data="previewTimes.map((t, i) => ({ i: i + 1, t }))" border size="small" max-height="360">
        <el-table-column prop="i" label="#" width="50" />
        <el-table-column prop="t" label="计划执行时间" />
      </el-table>
    </el-dialog>
    <template #footer>
      <el-button @click="emit('update:modelValue', false)">取 消</el-button>
      <el-button type="primary" :loading="submitting" @click="onSubmit">确 定</el-button>
    </template>
  </el-drawer>
</template>

<style scoped>
.group-title {
  font-size: 13px;
  font-weight: 600;
  color: #303133;
  margin: 4px 0 12px;
  padding-left: 8px;
  border-left: 3px solid #409eff;
}
.hint {
  font-size: 12px;
  color: #909399;
  line-height: 1.6;
}
.hint code {
  background: #f5f7fa;
  padding: 0 4px;
  border-radius: 3px;
}
.advanced {
  margin-top: 8px;
}
.full {
  width: 100%;
}
.expr-row {
  display: flex;
  gap: 8px;
  width: 100%;
}
.expr-row .el-input {
  flex: 1;
}
</style>
