# gojob Worker Java SDK（多模块）
> 本文的「兼容协议」= gojob 控制面刻意对齐的那套外部接口语义；对齐范围与逐条差异见根 README 的「兼容性说明」。


gojob 的 Java Worker SDK（Maven 多模块工程，`groupId=com.gojob`，`version=0.2.0`，Java 17）。

业务应用引入本 SDK、注册处理器后启动，即作为 gojob Server 的一个 worker 节点执行任务：

- 通信走 gojob 的 HTTP+JSON 数据面协议（`/worker/register`、`/worker/heartbeat`、`/worker/fetchTask`、`/worker/report`、`/worker/reportLogs`），与 Go SDK（仓库 `worker/` 目录）行为对等；鉴权为 `start()` 时用 `appId + password` 注册换取 worker token（服务端存 bcrypt 摘要；配了 `aesKey` 则口令走"一次性 challenge + AES-256-GCM"握手，不再明文上线），之后所有数据面请求只带 token；token 被服务端回 `401`（服务重启、票过期、项目改口令）时 SDK 自动重新注册并重放当次请求，多线程同时撞上也只注册一次；`report` 只被**认领该任务的那台 worker** 接受（`instanceId` 进程内固定，无需业务侧处理），越权或失联复活后的迟到回报会被服务端忽略；
- 用户体验对标兼容协议 worker：`Processor` 接口 + `ProcessResult`、按任务上下文取参（`jobParams` / `instanceParams`）、支持用 Java 类全名作为 processor 直接执行。

## 模块结构

| 模块 | artifactId | 说明 |
| --- | --- | --- |
| `gojob-worker-core/` | `com.gojob:gojob-worker-core` | 纯 Java SDK：`GoJobWorker` + `Processor` SPI，零框架依赖（仅 OkHttp/Jackson/slf4j-api）。任何 JVM 应用可直接使用 |
| `gojob-worker-spring-boot-starter/` | `com.gojob:gojob-worker-spring-boot-starter` | Spring Boot 3 自动装配：`gojob.worker.*` 配置项，自动把容器内所有 `Processor` bean 按类全名注册并启动/优雅关闭 worker |
| `examples/quarkus-worker/` | `com.gojob:quarkus-worker`（示例） | Quarkus 接线示例：`@Observes StartupEvent/ShutdownEvent` 管理 worker 生命周期（真实 `quarkus-arc`/`quarkus-core` 依赖，编译级演示） |

三种消费形态：**plain Java**（直接用 core）、**Spring Boot**（用 starter）、**Quarkus**（照抄示例的 CDI 接线，依赖 core）。

## 用法一：Plain Java

```xml
<dependency>
    <groupId>com.gojob</groupId>
    <artifactId>gojob-worker-core</artifactId>
    <version>0.2.0</version>
</dependency>
```

```java
OpenConfig config = new OpenConfig()
        .setServerAddress("http://127.0.0.1:7800") // gojob Server
        .setAppId("demo")                            // 需已在 Server 创建的应用
        .setPassword("demo123");

GoJobWorker worker = new GoJobWorker(config);

// 方式一：注册名字 + lambda，Server 端 Job 的 processor 填 "hello"
worker.registerProcessor("hello", ctx -> new ProcessResult(true, "hi"));

// 方式二：类全名，Server 端 processor 填 "com.acme.jobs.MyProcessor"，无需注册
//（要求实现 com.gojob.worker.Processor、有无参构造；实例进程内缓存复用，须无状态）

worker.start();
Runtime.getRuntime().addShutdownHook(new Thread(worker::close));
worker.awaitTermination(); // 阻塞，保持 main 存活
```

可运行示例见 `gojob-worker-core/src/test/java/com/gojob/worker/examples/QuickStart.java`（含 `DemoProcessor`）。

## 用法二：Spring Boot 3

```xml
<dependency>
    <groupId>com.gojob</groupId>
    <artifactId>gojob-worker-spring-boot-starter</artifactId>
    <version>0.2.0</version>
</dependency>
```

`application.yml`：

```yaml
gojob:
  worker:
    enable: true                 # 默认 true，置 false 整体关闭
    server-address: http://127.0.0.1:7800
    app-id: demo
    password: demo123
    worker-instance-id: ""       # 可选，本节点对外标识；默认 hostname-pid
    max-concurrency: 16
    heart-beat-seconds: 5
    fetch-wait-seconds: 30
```

处理器写成 Spring bean 即可，自动装配会**按 bean 类全名注册**——Server 端 Job 的
`processor` 字段（含兼容协议导入任务的 `processorInfo`）直接填该类的 FQCN：

```java
@Service
public class Hello implements com.gojob.worker.Processor {
    @Override
    public ProcessResult process(TaskContext ctx) {
        return new ProcessResult(true, "hello from spring");
    }
}
// Job 的 processor = "com.acme.jobs.Hello"
```

也可用 `@GoJobProcessor(name = "greeting")`（自带 `@Component` 语义）额外注册短名。
处理器走 Spring 单例而非 SDK 反射 new，可直接构造器注入其他 bean。
容器关闭时自动调用 `GoJobWorker.close()`（含 drain，见下）。
编译级示例见 starter 模块 `src/test/java/.../SpringDemoApplication.java`。

## 用法三：Quarkus（CDI）

见 `examples/quarkus-worker/`。核心接线：

```java
@ApplicationScoped
public class GoJobWorkerLifecycle {
    @Inject HelloProcessor helloProcessor;   // Processor 实现就是一个 CDI bean
    private volatile GoJobWorker worker;

    void onStart(@Observes StartupEvent event) {
        worker = new GoJobWorker(new OpenConfig()
                .setServerAddress(serverAddress).setAppId(appId).setPassword(password));
        // 按类全名注册：Server 端 processor 填 com.gojob.example.quarkus.HelloProcessor
        worker.registerProcessor(HelloProcessor.class.getName(), helloProcessor);
        worker.start();
    }

    void onStop(@Observes ShutdownEvent event) {
        worker.close(); // 优雅下线：停收单 + drain 运行中任务 + 释放资源
    }
}
```

## 配置项（OpenConfig / gojob.worker.*）

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `serverAddress` / `server-address` | 必填 | Server 地址，如 `http://host:7800`；**多副本可逗号写全**：`http://h1:7800,http://h2:7800` |
| `serverAddresses` / `server-addresses` | 空 | 集群各副本地址（列表形式）。与 `serverAddress` 合并去重，首个即启动时的首选地址；填了就不需要给 worker 挂 LB/VIP |
| `appId` / `app-id` | 必填 | 应用 ID |
| `password` | `""` | 应用密码 |
| `address` / `worker-instance-id` | `hostname-pid` | 本节点对外标识 |
| `maxConcurrency` / `max-concurrency` | 16 | 执行线程池大小，即并发任务上限。"一个 worker 同时跑 200 个任务"就是把它开到 200：拉取时 `max_task_num` 报的是当时空闲额度，服务端对该数不设上限，一次长轮询即可带回 200 条。额度满时 fetch 循环等"有额度释放"的信号（1s 只作兜底），不再固定睡一秒。注意这是 JVM 线程，200 条线程的成本远高于 Go 的 200 条 goroutine |
| `heartBeatSeconds` / `heart-beat-seconds` | 5 | 心跳间隔 |
| `fetchWaitSeconds` / `fetch-wait-seconds` | 30 | 长轮询单次阻塞秒数（服务端上限 120） |
| `sdkType` / `sdk-type` | `JAVA` | 心跳上报的 SDK 类型（统计页展示）：plain Java=`JAVA`，Spring Boot starter 默认 `JAVA_SPRINGBOOT`，Quarkus 示例 `JAVA_QUARKUS` |
| `tag` / `gojob.worker.tag` | 空 | 机器标签，随心跳上报，供任务分发策略 `SPECIFY` 用 `tagIn:` / `tagEquals:` 匹配 |
| `aesKey` / `gojob.worker.aes-key` | `""` | 注册口令加密的 key（64 位 hex，AES-256），须与服务端 `GOJOB_WORKER_AES_KEYS` 中的某一把一致。配了它，`password` 不再明文上线（注册先领一次性 challenge，再送 AES-256-GCM 密文）。**配了就一定加密**：服务端没启用时注册直接抛 `GoJobException` 并指出是哪一侧没配，不会静默退回明文。示例进程读 `GOJOB_WORKER_AES_KEY`。JCE 没有 HKDF，`internal/RegisterCipher` 照 RFC 5869 手写并在单测里钉住（含"用 Java 解开 Go 产出的黄金密文"这一条），口径与服务端一致 |

## 多副本：自寻址与 owner 跟随

Server 是多副本时，把副本地址都配上（`server-address: http://h1:7800,http://h2:7800`），**不需要再为 worker 挂 LB/VIP**。SDK 侧规则与 Go SDK 逐条对齐（实现见 `GoJobWorker.applyHint` + `internal/HttpTransport`）：

1. 启动时按列表顺序取第一个地址；任何一次"这台没答上话"（连不上、连接被 reset、5xx）就在其余已知地址上重放；业务错误（HTTP 200 且 `code!=0`，含 401 重注册）不换副本。
2. 心跳响应带 `cluster_hint`：`servers` 并进已知集合（**后来新起的副本不需要重启 worker 就能被发现**），`owner` 是本 app 调度归属副本的可达地址。
3. `owner` 只是首选地址：必须在已知集合内、且**连续 2 跳**指向它才迁移；中途提示消失（租约交接中、对方没配对外地址）会重新开始计数。
4. 迁过去连不上 → 自动退回已知副本继续收单回报，任务不丢。**这与兼容协议不同**：兼容协议的 `current_server` 是唯一通路，worker 连续 3 次找不到 owner 会本地杀死秒级任务。
5. 地址变更时会**放弃挂在旧地址上的那一次长轮询**（不等它自然到点）；被放弃的任务由 Server 侧超时回收重派，属"至少一次 + 回报判重"。
6. 读超时按路径分两档：长轮询 `max(60, fetch-wait+30)`，心跳/注册/回报/日志 **15s**。第二档是必需的——`SIGSTOP` 冻结（长 GC、虚机挂起）的副本 TCP 不断、请求不返回，只有超时能认出它，而集群提示正是从心跳响应下来的。

时延口径：副本崩溃（RST）≈ 10s（2 跳确认）；副本冻结 ≈ 15s 认出 + 2 跳确认 ≈ 25s。实机验收：`scripts/verify-java-routing.sh`（2 副本 + 4 个 Java worker，含收敛、防抖、冻结接管、无双跑）。

## 任务执行日志（TaskContext 日志 API）

处理器内直接写日志，SDK 可靠上传到 Server，控制台「任务日志」页增量查看：

```java
worker.registerProcessor("orderSync", ctx -> {
    ctx.info("开始同步订单，taskId={}，第 {} 次尝试", ctx.getTaskId(), ctx.getAttempts()); // {} 占位符
    ctx.warn("下游接口超时，准备降级");
    ctx.error("同步失败: {}", e.toString());
    return new ProcessResult(true, "done");
});
```

- 级别 `debug/info/warn/error`，按任务配置的 `log_level`（DEBUG|INFO|WARN|ERROR|OFF，默认 INFO）在 worker 侧过滤；
- **不丢不重的保证**：每条日志带任务内单调递增 `seq`；SDK 本地 FIFO 缓冲，只有收到 Server `code=0` 的 ack 才出队，失败整批放回队首无限重试（at-least-once）；Server 按 `(task_id, seq)` 唯一索引 `ON CONFLICT DO NOTHING` 去重——重发不会产生重复日志；
- 任务结束**先 flush 本任务日志再回报结果**，控制台看到终态实例时其日志一定已可查；`close()` 排空执行池后还有 15s 兜底 flush；
- 缓冲上限 50000 条，极端积压丢最旧并告警（防止 OOM 的最后一道闸）。

## 多任务执行类型（BROADCAST / MAP / MAP_REDUCE）

接口形状与兼容协议完全一致，因此兼容协议的 `MapProcessor` / `MapReduceProcessor` 实现基本可以原样搬过来：

```java
public class MapSplit implements MapProcessor {
    @Override
    public ProcessResult process(TaskContext ctx) throws Exception {
        if (ctx.isRootTask()) {                                  // 根任务：算分片、派生
            List<Object> parts = List.of(Map.of("word", "alpha"), Map.of("word", "beta"));
            ctx.map(parts, "UPPER");                             // 返回 server 分配的层次 ID
            return new ProcessResult(true, "已派生 " + parts.size() + " 片");
        }
        Payload p = ctx.getSubTask(Payload.class);               // 子任务：干真正的活
        return new ProcessResult(true, p.getWord().toUpperCase(Locale.ROOT));
    }
}

public class MrSum implements MapReduceProcessor {
    @Override
    public ProcessResult process(TaskContext ctx) { /* 同上，子任务分支返回自己的数 */ }

    @Override
    public ProcessResult reduce(TaskContext ctx, List<TaskResult> results) {
        // results 里失败的那几行 success=false、result 是错误原因：
        // 部分失败要不要兜住由这里决定，reduce 的返回值就是实例的最终结论
        long sum = results.stream().filter(TaskResult::isSuccess).mapToLong(r -> Long.parseLong(r.getResult())).sum();
        return new ProcessResult(true, "总和=" + sum);
    }
}
```

- `BROADCAST` 不需要实现任何额外接口——Server 按在线节点快照每台派生一份，节点失联时那一份判失败、不换机重跑；
- `ctx.map(...)` 只能在执行类型为 `MAP` / `MAP_REDUCE` 的实例里调，`STANDALONE` 里调会抛 `GoJobException` 并点名原因；子任务里继续 `map` 就是多级 Map；
- 任务配成 `MAP_REDUCE` 而处理器没实现 `MapReduceProcessor`，那条 reduce 任务会明确失败并说明"未实现 worker 的 Reducer 接口"，不会静默跳过；
- `MapProcessor.RECOMMEND_BATCH_SIZE = 200`：单次 `map` 超过它打一条 WARN（兼容协议同值），硬上限 5000 在 Server 侧；
- 与兼容协议的三处差别（负载是 JSON 而非 JDK 序列化、保留任务名**报错**而不是悄悄改名、reduce 不钉回根任务那台机器）见 `../worker/README.md` 同名小节，Go/Java 两侧行为一致。

## 公开方法语义（GoJobWorker）

- `start()`：先用 `appId + password` 调 `/worker/register` 换 worker token（配了 `aesKey` 时先调 `/worker/challenge` 领凭证、口令随密文送上），再启动心跳线程 + fetch 线程（均守护线程）与固定大小执行池，立即返回。重复调用抛 `IllegalStateException`。
  启动期注册按两类失败分治（与 Go SDK 同语义）：**连不上**（reset/refused/超时）阶梯退避重试 200ms→1s→2s→5s 带抖动、累计 2 分钟后才抛错——整池 worker 同时冷启动时注册要过 BCrypt，服务端 accept 队列会溢出（实测 200 节点同机、`somaxconn=128` 时每轮 0~24 个连接被 reset），就此退出等于节点永久缺席；**服务端明确拒绝**（口令不对、要求加密注册、服务端没配 key）立即抛错不重试，因为重试不会让配置变对。
- 心跳间隔带 **±25% 相位抖动**（`heartBeatSeconds ≥ 2` 时生效）：心跳是 worker 侧唯一高频写库的请求，整池同相位时每到点齐射一次，会把 sqlite 单写者排满（实测单条 upsert 237~338ms），同通道上的任务认领被顶到 1 秒开外。
- `stopReceivingNewTasks()`：**优雅下线核心开关（drain 第一步）**。置位内部 `AtomicBoolean` 标志后，fetch 循环立即不再发起新的 `/worker/fetchTask` 拉取；**已提交执行中的任务不受任何影响，会继续正常执行并向 Server 回报结果**。方法立即返回，可用 `isReceiving()` 确认状态。三种形态语义相同：
  - plain Java：直接对 `worker` 实例调用；
  - Spring Boot：向容器注入 `GoJobWorker` bean 后调用（`close()` 已挂为 destroyMethod，应用停止自动 drain）；
  - Quarkus：`ShutdownEvent` 里 `close()` 即包含 stopReceiving + drain，也可先手动 `stopReceivingNewTasks()` 观察再关。
- `resumeReceivingNewTasks()`：恢复收单（与 stop 成对，用于灰度/回滚场景）。
- `isReceiving()`：当前是否接收新任务。
- `awaitTermination()`：阻塞直到 `close()` 完成，main 里保活用。
- `close()`：幂等。执行 `stopReceivingNewTasks` 语义 → 取消在途 fetch 长轮询 → **无限期等待执行池排空**（所有运行中任务执行并回报完毕）→ 释放 HTTP 资源。第二次调用会阻塞等待首次调用完成。建议挂到 JVM shutdown hook / 容器生命周期上。

## 协议与容错行为

- 被服务端停止实例：`fetchTask` / `heartbeat` 响应里的 `cancel_task_ids` 会标记该任务并 `interrupt()` 其执行线程；若指令在开跑前就到，则直接跳过执行并回报 `STOPPED`（结果列 `cancelled before start`，Go SDK 同文案）。停止的任务不计失败、不重试。
- **业务侧检查停止有三种写法**（`TaskContext` 提供，三种 Java 形态通用，因为处理器始终跑在 core 自己的线程池上，容器只负责发实例）：

  ```java
  for (Batch b : batches) {
      ctx.checkStopped();                                   // ③ 最省事：已停止直接抛 TaskStoppedException
      if (ctx.isStopped()) return new ProcessResult(false, "cancelled");   // ② 读 worker 侧停止标记
      if (Thread.currentThread().isInterrupted()) return …; // ① 原生中断标志
      doOne(b);
  }
  ```

  ①③ 可以只留 ③。**推荐 ②/③ 的理由**：JVM 抛 `InterruptedException` 时会**顺手清掉中断标志**（本机 JDK 25 实测：虚拟线程 `Thread.sleep` 被 interrupt 后 `isInterrupted()` 返回 false），所以 `catch (InterruptedException e) {}` 一写，① 就永久失效，而 `isStopped()` 读的是 worker 侧的 taskId 标记，不受吞异常影响。实机验证（真 server + 真 worker，处理器故意吞掉中断）：走 `isStopped()` 与 `checkStopped()` 的两个任务都在停止指令到达后的下一个检查点退出，结果列分别是 `第 N 轮就停了` 与 `TaskContext$TaskStoppedException: task 3 stopped by server request`；而完全不检查的纯 CPU 自旋跑满了 8 秒才结束——协作式取消对它是无能为力的。
- 派活给别的线程时（`@Async`、自己的 `ExecutorService`、Quarkus `ManagedExecutor`、`parallelStream`）中断**传不过去**：把 `ctx.isStopped()` 当参数传进子任务，或对 `Future` 调 `cancel(true)` 并在收尾 `shutdownNow()`。
- 普通 JDBC、`InputStream.read`、OkHttp 同步 `execute()` 在**平台线程**上不受 `interrupt()` 影响，要停它们得靠 `Statement.cancel()` / `soTimeout` / `Call.cancel()`。**换到虚拟线程上这条限制基本消失**，见下节。
- 所有请求 `POST`、`Content-Type: application/json`，按 `internal/protocol/protocol.go` 的 snake_case tag 逐字段 `@JsonProperty` 映射。
- 响应信封 `{code, message, data}`：HTTP 非 200 或 `code != 0` 均视为失败。
- fetch 失败退避 2s 重试；特例：`code=500` 且 message 含 `no alive worker for app`（刚启动、首次心跳尚未生效的瞬时状态）按 debug 级别静默、1s 短退避重试。
- 心跳失败仅告警，下个周期自然重试。
- 任务失败（异常或 `ProcessResult.success=false`）直接回报 `success=false`；若 Server 侧还有剩余重试次数会自动重新入队，**SDK 不做本地重试**。回报请求本身失败会退避重试 3 次，仍失败则依赖 Server 对失联 RUNNING 任务的超时回收。
- OkHttp `readTimeout` 强制 `max(60, fetchWaitSeconds + 30)` 秒：长轮询时服务端会挂起 `wait_seconds`（外加最多 5s 余量）才响应，读超时若不大于 wait_seconds 会把空轮询掐断导致无限重试风暴，因此该约束在构造函数中兜底，不可配错。

## 运行时指标上报（节点监控）

`internal/RuntimeMetrics` 在每次心跳前填好 `HeartbeatReq` 的 runtime 字段，控制台「系统管理 → 节点监控」的 worker 行即来自这里；三种接入形态（plain / Spring Boot starter / Quarkus 示例）都经由同一个 `GoJobWorker`，因此**无需任何额外配置**，也不引入新依赖（全部走 JDK 自带 MXBean）：

| 字段 | 取法 |
| --- | --- |
| `gc_count` / `gc_pause_ms` | `GarbageCollectorMXBean` 各收集器求和（累计暂停毫秒）；返回负值的收集器跳过 |
| `threads` | `ThreadMXBean.getThreadCount()` |
| `open_fds` | `com.sun.management.UnixOperatingSystemMXBean.getOpenFileDescriptorCount()` |
| `cpu_percent` | `getProcessCpuTime()` 差值 / 墙钟时间，100 = 吃满一个核（与 Go 侧同口径，便于跨语言横向比较） |
| `rss_mb` | **恒为 0**：JDK 没有可移植的 per-process RSS 取法（`getPhysicalMemorySize` 是机器内存，不是进程），页面显示"—" |
| `running_tasks` | 已占用的并发额度 = `maxConcurrency - slots.availablePermits()` |

非 HotSpot JVM 拿不到 `com.sun.management.*` 时，`os == null` 分支直接返回，相关项整体报 0（不误报）。既有的 `cpu_load`（`getSystemLoadAverage`）、`memory_used/max`（堆用量与 `-Xmx`）保持不变。

## 与兼容协议任务导入的兼容

gojob Server 的 `/openapi/job/import` 支持导入兼容协议控制台导出的任务，其中 `processorInfo`（兼容协议里可以是 `BasicProcessor` 实现类全名或带 `#` 分割的内联类名）会被写进 Job 的 `processor` 字段下发给 worker。core 的解析顺序：

1. `registerProcessor(name, processor)` 手工注册表命中（Spring starter / Quarkus 示例的自动注册即走这条路，实例是容器单例）；
2. 未命中则把字符串当 Java 类全名 `Class.forName`（失败再走线程上下文 ClassLoader），用无参构造实例化并缓存复用；
3. 都失败 → 该任务按失败回报（"processor not found"），由 Server 按重试策略处理。

因此从兼容协议迁移过来的任务，只要把原 worker 工程里的 Processor 实现类（实现 `com.gojob.worker.Processor`、保留无参构造）随应用打包，`processorInfo` 原样导入即可执行，无需改 Job 配置。注意：处理器实例是进程内单例，请保持无状态；每次执行的入参通过 `TaskContext`（`getJobParams*` / `getInstanceParams*`）获取。

## 构建

```bash
cd sdk-java
mvn -DskipTests package   # 编译 + 打包全部模块（core / starter / quarkus 示例）
mvn test                  # core 示例编译 + starter 编译级示例
mvn install               # 供本机其他工程依赖
```
