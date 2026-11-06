/** 把 Date 渲染为本地可读字符串 */
function pad(n: number) {
  return String(n).padStart(2, '0')
}

function fmtDate(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes(),
  )}:${pad(d.getSeconds())}`
}

/** 把后端 RFC3339 时间格式化为本地可读字符串 */
export function formatTime(iso: string | undefined | null): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return fmtDate(d)
}

/** 毫秒时间戳（如任务日志 log_time）→ 本地可读字符串 */
export function formatMillis(ms: number | undefined | null): string {
  if (!ms) return '-'
  const d = new Date(ms)
  if (Number.isNaN(d.getTime())) return '-'
  return fmtDate(d)
}

/** 实例状态 → Element tag 类型配色 */
export function statusTagType(
  status: string,
): 'success' | 'danger' | 'primary' | 'info' | 'warning' {
  switch (status) {
    case 'SUCCEED':
      return 'success'
    case 'FAILED':
      return 'danger'
    case 'STOPPED':
      return 'warning'
    case 'STOPPING':
      return 'warning'
    case 'RUNNING':
      return 'primary'
    default:
      return 'info' // WAITING 及未知状态灰色
  }
}

/** 实例状态 → 中文名称（未知状态原样显示，不藏数据） */
export function statusLabel(status: string): string {
  switch (status) {
    case 'WAITING':
      return '等待中'
    case 'RUNNING':
      return '执行中'
    case 'SUCCEED':
      return '成功'
    case 'FAILED':
      return '失败'
    case 'STOPPED':
      return '已停止'
    case 'STOPPING':
      return '停止中'
    default:
      return status || '-'
  }
}

/** 实例状态 → 状态胶囊配色（颜色始终配文字，不单靠颜色） */
export function statusPillClass(status: string): string {
  switch (status) {
    case 'SUCCEED':
      return 'gj-pill--ok'
    case 'FAILED':
      return 'gj-pill--bad'
    case 'RUNNING':
      return 'gj-pill--run'
    case 'STOPPED':
    case 'STOPPING':
      return 'gj-pill--warn'
    default:
      return 'gj-pill--idle'
  }
}

/** 实例触发来源 → 中文名称（未知来源原样显示，不藏数据） */
export function sourceLabel(source?: string): string {
  switch (source) {
    case 'API':
      return '手动'
    case 'CRON':
      return '定时'
    case 'FIXED_RATE':
      return '固定频率'
    case 'FIXED_DELAY':
      return '固定延迟'
    case 'DEPENDENCY':
      return '依赖'
    case 'WORKFLOW':
      return '工作流'
    default:
      return source || '定时'
  }
}

/** Worker SDK 类型 → Element tag 类型配色 */
export function sdkTagType(sdkType: string): 'success' | 'primary' | 'warning' | 'info' {
  switch (sdkType) {
    case 'GO':
      return 'success'
    case 'JAVA':
      return 'primary'
    case 'JAVA_SPRINGBOOT':
      return 'warning'
    default: // JAVA_QUARKUS 及未知类型
      return 'info'
  }
}
