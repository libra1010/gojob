# gojob 控制台（dashboard）
> 本文的「兼容协议」= gojob 控制面刻意对齐的那套外部接口语义；对齐范围与逐条差异见根 README 的「兼容性说明」。
gojob 的 Web 管理台，以兼容协议的 4.x 官方控制台为蓝本，登录后落地到统计总览。技术栈：**Vue 3 + TypeScript + Element Plus + Pinia + vue-router + axios**，Vite 构建，pnpm 管理。

## 目录结构

```
src/
├── api/          # 接口封装：http.ts(axios 实例+拦截器)、app/job/instance/stats/user.ts、types.ts
├── views/        # 页面：LoginView / OverviewView / AppListView / JobListView / InstanceListView / InstanceLogView
├── layouts/      # MainLayout.vue（顶部导航 + 内容区）
├── router/       # 路由与登录守卫（X-Gojob-Token）
├── stores/       # Pinia：auth（登录态/token）
├── components/   # 复用组件：JobFormDrawer / WorkersDrawer 等
└── utils/        # format.ts 等
```

## 页面

| 路由 | 页面 | 说明 |
| --- | --- | --- |
| `/login` | 登录 | 账号密码换 token |
| `/` | 统计总览 | 落地页：账号数/在线 Worker/任务与项目总数/今日执行与成功率/对外地址/数据库类型；在线 Worker 表（IP + SDK 类型 GO/JAVA/JAVA_SPRINGBOOT/JAVA_QUARKUS）、常用项目卡片、最近实例 |
| `/apps` | 项目管理 | el-card 卡片列表，含任务数/在线节点/创建时间，可新建/编辑/删除/查看节点/进入任务 |
| `/apps/:appId/jobs` | 任务列表 | 过滤条只有**任务 ID + 关键字**两个条件（对齐兼容协议，无时间范围）；分页 `prev, pager, next, total`（后端 index 0 基，`page_size=0` 表示一次给全量，供上游候选下拉用）；列：任务 ID、任务名称、描述、处理器、**定时信息**、**状态 = el-switch**（绿 `#13ce66`/红 `#ff4949`，只有后端确认成功才改本地值，失败时开关留在原位不显示假状态）、日志级别、跳过策略、下次触发时间；操作列 = 编 辑 / 运 行 / 运行记录 / **更多**下拉（**参数运行**：弹实例参数输入框，只影响这一次运行；**复制**：把配置灌进「新建」表单、任务名加 `_copy`，不落库；**导出**：通用任务包（camelCase JobDTO），可复制/下载，能原样粘回导入框；删除）；新建/编辑抽屉含全部任务字段（执行类型/处理器类型/实例参数/双重试/并发度/实例数上限/最大 Worker 数/指定机器/调度策略/分发策略（健康优先·随机·指定机器·轮询）/时间类型（CRON·固定频率·固定延迟·每日固定间隔·**依赖触发**·API，可预览最近 20 次触发时间）），首列是多选框，工具栏「批量导出」按 **选中 / 本项目全部** 两种范围出一份任务包（单任务导出仍走行内「更多」）；「导 入」接受 JSON 对象或数组，且可一次选多个 .json 文件（逐个解析后合并成待导清单，逐条返回成败与原因）。选「依赖触发」时在「定时信息」区块内直接出现**上游任务**单选（候选自动排除自身与自己的下游以免成环），「定时信息」列显示该任务在等哪个上游。脚本任务在抽屉里选完处理器类名后有专门的**脚本来源**入口（脚本库文件名 + 命令行参数 / 内联脚本正文），两种写法都落到「任务参数」这一个字段里，不另存 |
| `/jobs/:jobId/instances` | 实例列表 | 状态（含 STOPPING/STOPPED）/来源/进度/重试，每行「日志」入口，未终态实例可「停止」（按项目授权显隐），10s 自动刷新 |
| `/instances/:instanceId/logs` | 实例日志 | 按 task 查看上报的执行日志，自动跟随最新 |
| `/system/users` | 用户管理 | 账号增删改、角色、项目授权范围、禁用/重置密码（仅超管可见可写） |
| `/system/login-logs`、`/system/operation-logs` | 审计日志 | 登录（含失败原因）与写操作审计，`ADMIN`/`SUPER_ADMIN` 可见 |
| `/system/nodes` | 节点监控 | `ADMIN`/`SUPER_ADMIN` 可见；调度中心副本表（进程 CPU%、RSS/堆、协程、GC 次数与累计暂停、活跃/空闲 HTTP 连接、长轮询在途、QPS，各列带最近 10 分钟曲线）+ Worker 节点表（心跳上报的 runtime 指标与在跑任务数）。取不到的指标显示"—"（macOS 无 `/proc` ⇒ RSS/fd；JVM 无可移植 RSS），刷新周期跟着接口的 `interval_ms` 走，可关自动刷新 |

## 开发

需要 pnpm（本机在 `/opt/homebrew/bin`）。

```bash
cd dashboard
pnpm install
pnpm dev        # http://127.0.0.1:5173
```

开发时 `/openapi/*` 经 Vite 代理转发到 Server，代理目标由环境变量 `VITE_API_TARGET` 指定，默认 `http://127.0.0.1:7789`：

```bash
VITE_API_TARGET=http://127.0.0.1:7700 pnpm dev
```

## 构建

```bash
pnpm build      # vue-tsc 严格类型检查 + vite build，产物 dist/
pnpm preview    # 预览构建产物
```

生产部署见根目录 `docker-compose.yml` 的 `dashboard` 服务：Nginx 托管静态资源并反代 `/openapi` 到 `GOJOB_UPSTREAM`（默认 `http://gojob-server:7700`），模板见 `../deploy/nginx-dashboard.conf.template`。
