package com.gojob.worker;

import com.fasterxml.jackson.databind.JsonNode;
import com.gojob.worker.dto.ChallengeReq;
import com.gojob.worker.dto.ChallengeResp;
import com.gojob.worker.dto.ClusterHint;
import com.gojob.worker.dto.FetchTaskReq;
import com.gojob.worker.dto.FetchTaskResp;
import com.gojob.worker.dto.HeartbeatReq;
import com.gojob.worker.dto.MapTaskReq;
import com.gojob.worker.dto.MapTaskResp;
import com.gojob.worker.dto.RegisterReq;
import com.gojob.worker.dto.RegisterResp;
import com.gojob.worker.dto.ReportTaskReq;
import com.gojob.worker.dto.TaskResultsReq;
import com.gojob.worker.dto.TaskResultsResp;
import com.gojob.worker.dto.Tokenized;
import com.gojob.worker.dto.TaskDTO;
import com.gojob.worker.internal.HttpTransport;
import com.gojob.worker.internal.RegisterCipher;
import com.gojob.worker.internal.LogReporter;
import com.gojob.worker.internal.RuntimeMetrics;
import com.gojob.worker.internal.ServerException;
import com.gojob.worker.internal.TaskLogger;
import java.io.IOException;
import java.lang.management.ManagementFactory;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.Semaphore;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.Consumer;
import okhttp3.Call;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * gojob Java Worker SDK 主类：注册处理器后 start()，作为 worker 通过 HTTP 协议
 * 向 gojob Server 心跳、长轮询拉任务、执行并回报结果。
 *
 * <pre>{@code
 * OpenConfig cfg = new OpenConfig()
 *         .setServerAddress("http://127.0.0.1:7800")
 *         .setAppId("demo")
 *         .setPassword("demo123");
 * GoJobWorker worker = new GoJobWorker(cfg);
 * worker.registerProcessor("hello", ctx -> new ProcessResult(true, "hi"));
 * worker.start();
 * worker.awaitTermination(); // 直到 close()
 * }</pre>
 *
 * <p>优雅下线：{@link #stopReceivingNewTasks()} 只停止拉取新任务，已提交执行中的
 * 任务继续跑完并正常回报；{@link #close()} 会等待这批任务排空（drain）。
 */
public final class GoJobWorker implements AutoCloseable {

    public static final String VERSION = "gojob-java/0.2.0";

    private static final Logger log = LoggerFactory.getLogger(GoJobWorker.class);

    /** 心跳/注册/回报/日志这类短请求的读超时秒数；见构造函数里的说明。 */
    private static final int BEAT_TIMEOUT_SECONDS = 15;

    /**
     * owner 要连续被确认多少跳才迁移长轮询。取 2 的理由见 {@link #applyHint}：
     * 宁可慢一跳，也不要被一个还没刷到新租约的副本甩走。与 Go SDK 的 ownerConfirmBeats 同值。
     */
    private static final int OWNER_CONFIRM_BEATS = 2;

    private final OpenConfig config;
    private final String instanceId;
    private final HttpTransport transport;
    private final LogReporter logReporter;
    /** JVM 运行时指标（GC/线程/fd/进程 CPU），随心跳上报给节点监控页。 */
    private final RuntimeMetrics runtimeMetrics = new RuntimeMetrics();
    /**
     * 数据面凭证：start() 时用项目口令换取，之后所有请求只带它。
     * 服务端把 token 与 (app_id, instance_id) 绑定并滑动续期，401 即重新注册。
     */
    private volatile String token;
    /** 服务端票是无状态签名的、到点即失效（服务端没有可续的会话记录），所以有效期由本节点自己盯着。 */
    private volatile long tokenTtlMillis = 10 * 60 * 1000L;
    private volatile long tokenExpireAtMillis;
    private final Object registerLock = new Object();

    /** 手工注册的处理器表：name -> Processor。 */
    private final Map<String, Processor> registry = new ConcurrentHashMap<>();
    /** 类全名解析出的处理器实例缓存：className -> Processor（无参构造单例）。 */
    private final Map<String, Processor> classCache = new ConcurrentHashMap<>();

    /** 并发额度 = 空闲执行线程数；fetch 的 max_task_num 即 availablePermits()。 */
    private final Semaphore slots;
    /**
     * 额度释放信号（容量 1，满了就合并）：fetch 循环在"额度已满"时等它，而不是死睡 1 秒。
     * 与 Go SDK 的 {@code slotFree} 同语义。
     */
    private final LinkedBlockingQueue<Object> slotFree = new LinkedBlockingQueue<>(1);
    private final ExecutorService taskPool;
    private final ScheduledExecutorService heartbeatPool;

    private final AtomicBoolean receiving = new AtomicBoolean(true);
    private final AtomicBoolean started = new AtomicBoolean(false);
    private final AtomicBoolean closed = new AtomicBoolean(false);
    private final CountDownLatch terminationLatch = new CountDownLatch(1);

    private volatile Thread fetchThread;
    /** 正在执行的任务：taskId -> 执行线程，服务端停止指令靠它 interrupt。 */
    private final Map<Long, Thread> runningThreads = new ConcurrentHashMap<>();
    /** 已被请求停止的 taskId，决定回报 STOPPED 而非 FAILED。 */
    private final Set<Long> stopRequested = ConcurrentHashMap.newKeySet();
    /** 当前正在阻塞的 fetch 长轮询请求，close 或地址变更时 cancel 以便快速退出。 */
    private volatile Call activeFetchCall;
    private volatile boolean shutdownRequested;
    /** owner 提示的确认状态：只有心跳线程读写（心跳池是单线程），所以不需要锁。 */
    private String hintOwner = "";
    private int hintSeen;

    public GoJobWorker(OpenConfig config) {
        Objects.requireNonNull(config, "config");
        config.applyDefaults();
        this.config = config;
        this.instanceId = config.getAddress() + "-" + System.nanoTime();
        this.slots = new Semaphore(config.getMaxConcurrency());
        // 长轮询的读超时必须严格大于 wait_seconds（服务端还会多挂 5s），这里再加 30s 余量，下限 60s。
        int readTimeoutSeconds = Math.max(60, config.getFetchWaitSeconds() + 30);
        // 其余请求（心跳/注册/回报/日志）走 15s 短超时：被 SIGSTOP 冻结的副本不会断 TCP，
        // 只有超时能认出它，而集群路由提示正是从心跳响应下来的——挂在僵尸地址上就等于追不到新 owner。
        this.transport = new HttpTransport(config.getServerAddresses(), readTimeoutSeconds, BEAT_TIMEOUT_SECONDS);
        // 首选地址一变（故障转移或迁到 owner），挂在旧地址上的那次长轮询就没有意义了
        this.transport.onRotate(this::abandonActiveFetch);
        this.logReporter = new LogReporter(this::call, instanceId, config.getAppId());
        ThreadFactory factory = runnable -> {
            Thread t = new Thread(runnable, "gojob-worker-task");
            t.setDaemon(true);
            return t;
        };
        this.taskPool = Executors.newFixedThreadPool(config.getMaxConcurrency(), factory);
        this.heartbeatPool = Executors.newSingleThreadScheduledExecutor(runnable -> {
            Thread t = new Thread(runnable, "gojob-worker-heartbeat");
            t.setDaemon(true);
            return t;
        });
    }

    // ---------- 处理器注册与解析 ----------

    /** 注册处理器；Server 端 Job 的 processor 字段填 {@code name} 即路由到此实现。重复注册抛异常。 */
    public GoJobWorker registerProcessor(String name, Processor processor) {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(processor, "processor");
        if (registry.putIfAbsent(name, processor) != null) {
            throw new IllegalArgumentException("duplicate processor name: " + name);
        }
        return this;
    }

    /** 解析顺序：注册表 -> Java 类全名（Class.forName + 无参构造，实例缓存）。找不到返回 null。 */
    private Processor resolveProcessor(String name) {
        if (name == null || name.isEmpty()) {
            return null;
        }
        Processor p = registry.get(name);
        if (p != null) {
            return p;
        }
        return classCache.computeIfAbsent(name, GoJobWorker::loadProcessorClass);
    }

    private static Processor loadProcessorClass(String className) {
        try {
            Class<?> clazz;
            try {
                clazz = Class.forName(className);
            } catch (ClassNotFoundException e) {
                // 兜底走线程上下文 ClassLoader（容器/Boot fat-jar 场景）
                ClassLoader tcl = Thread.currentThread().getContextClassLoader();
                if (tcl == null) {
                    throw e;
                }
                clazz = Class.forName(className, true, tcl);
            }
            Object instance = clazz.getDeclaredConstructor().newInstance();
            if (!(instance instanceof Processor processor)) {
                log.error("class {} is not a com.gojob.worker.Processor", className);
                return null;
            }
            return processor;
        } catch (Throwable t) {
            log.warn("cannot load processor class {}: {}", className, t.toString());
            return null;
        }
    }

    // ---------- 生命周期 ----------

    /** 启动心跳线程与拉取线程（均为守护线程），立即返回。 */
    public synchronized void start() {
        if (!started.compareAndSet(false, true)) {
            throw new IllegalStateException("worker already started");
        }
        log.info("gojob worker starting: servers={} app={} address={} instanceId={} version={} sdkType={}",
                config.getServerAddresses(), config.getAppId(), config.getAddress(), instanceId, VERSION,
                config.getSdkType());
        try {
            registerAtStartup();
        } catch (IOException e) {
            started.set(false);
            throw new GoJobException("worker register failed, check app_id/password: " + e.getMessage(), e);
        }
        logReporter.start();
        // 首次心跳立即发一次，尽量赶在 fetch 之前，避免 "no alive worker" 瞬时错误
        heartbeatPool.execute(this::heartbeatOnce);
        scheduleHeartbeat();
        fetchThread = new Thread(this::fetchLoop, "gojob-worker-fetch");
        fetchThread.setDaemon(true);
        fetchThread.start();
    }

    /**
     * 停止接收新任务（优雅下线第一步）：fetch 循环不再发起新的拉取请求；
     * 已提交执行中的任务不受影响，会继续执行并正常回报。
     * 可再次 {@link #resumeReceivingNewTasks()} 恢复。
     */
    public void stopReceivingNewTasks() {
        receiving.set(false);
        log.info("worker stopped receiving new tasks; running tasks keep going");
    }

    /** 恢复接收新任务（配合 {@link #stopReceivingNewTasks()} 使用）。 */
    public void resumeReceivingNewTasks() {
        receiving.set(true);
        log.info("worker resumed receiving new tasks");
    }

    /** 当前是否在接收新任务。 */
    public boolean isReceiving() {
        return receiving.get();
    }

    /** 阻塞当前线程，直到 worker 完成 {@link #close()}（示例 main 里保持进程存活用）。 */
    public void awaitTermination() {
        try {
            terminationLatch.await();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    /**
     * 关闭 worker：先 {@link #stopReceivingNewTasks()}，取消在途 fetch 长轮询，
     * 然后无限期等待执行池排空（运行中任务全部执行完并完成回报），最后释放 HTTP 资源。
     * 幂等；重复调用会阻塞等待首次调用完成。
     */
    @Override
    public void close() {
        if (!closed.compareAndSet(false, true)) {
            awaitTermination();
            return;
        }
        log.info("gojob worker closing: draining {} running task(s)",
                config.getMaxConcurrency() - slots.availablePermits());
        shutdownRequested = true;
        receiving.set(false);
        heartbeatPool.shutdownNow();
        Call call = activeFetchCall;
        if (call != null) {
            call.cancel();
        }
        Thread ft = fetchThread;
        if (ft != null) {
            ft.interrupt();
        }
        // 关键语义：不掐运行中任务，等它们执行完并回报
        taskPool.shutdown();
        boolean interrupted = false;
        for (;;) {
            try {
                if (taskPool.awaitTermination(1, TimeUnit.SECONDS)) {
                    break;
                }
            } catch (InterruptedException e) {
                interrupted = true;
            }
        }
        // 执行池排空后兜底 flush 残留日志，再停发送线程
        logReporter.flush(15_000);
        logReporter.stop();
        try {
            if (ft != null) {
                ft.join(TimeUnit.SECONDS.toMillis(config.getFetchWaitSeconds() + 10L));
            }
        } catch (InterruptedException e) {
            interrupted = true;
        }
        transport.close();
        terminationLatch.countDown();
        log.info("gojob worker stopped: app={} address={}", config.getAppId(), config.getAddress());
        if (interrupted) {
            Thread.currentThread().interrupt();
        }
    }

    // ---------- 数据面凭证 ----------

    /** 启动期注册对"连不上"的止损时间（毫秒），与 Go SDK 的 registerGiveUp 同值。 */
    private static final long REGISTER_GIVE_UP_MS = 120_000L;
    private static final long[] REGISTER_BACKOFF_MS = {200L, 1_000L, 2_000L, 5_000L};

    /**
     * 启动期注册：连不上就退避重试，服务端明确拒绝则立即失败。与 Go SDK 的
     * {@code registerAtStartup} 同语义（分类也一致：{@link ServerException} 是"答了但说不行"）。
     *
     * <p>必须有重试：注册是整条链路上唯一要过 BCrypt 校验项目口令的请求，一次几十毫秒 CPU。
     * 整池 worker 同时冷启动会把服务端的 accept 队列打满——实测 200 节点同机启动、
     * 队列上限 128 时，每轮稳定有 6~23 个节点在 /worker/register 上吃到 connection reset。
     * 就此退出的节点会永久缺席，而其余节点跑得好好的，现场看起来"调度没问题"。
     */
    private void registerAtStartup() throws IOException {
        long deadline = System.currentTimeMillis() + REGISTER_GIVE_UP_MS;
        for (int attempt = 0; ; attempt++) {
            try {
                register();
                if (attempt > 0) {
                    log.info("worker 注册在第 {} 次尝试后成功", attempt + 1);
                }
                return;
            } catch (ServerException e) {
                throw e; // 口令不对/要求加密注册/服务端没配 key：重试不会让它变对
            } catch (IOException e) {
                long left = deadline - System.currentTimeMillis();
                if (left <= 0) {
                    throw new IOException("注册持续连不上（重试 " + (attempt + 1) + " 次，超过 "
                            + REGISTER_GIVE_UP_MS + "ms 止损）", e);
                }
                // 抖动是必需的：整池节点同时启动，不错峰就会在每个阶梯上再撞一次队列
                long wait = REGISTER_BACKOFF_MS[Math.min(attempt, REGISTER_BACKOFF_MS.length - 1)];
                wait += (long) (Math.random() * (wait / 2));
                if (wait > left) {
                    wait = left;
                }
                log.warn("worker 注册连不上({})，{}ms 后重试", e.toString(), wait);
                try {
                    Thread.sleep(wait);
                } catch (InterruptedException ie) {
                    Thread.currentThread().interrupt();
                    throw new IOException("注册重试被中断", ie);
                }
            }
        }
    }

    /**
     * 用项目口令换 token：整条数据面只有这一刻带口令。
     * 口令被改或项目被删时服务端会吊销该项目的票，节点只能凭新口令重新注册。
     *
     * <p>配了 {@code aesKey} 时口令不再明文上线：先领一张一次性 challenge，再把它连同
     * 口令一起封进 AES-256-GCM 密文。配了 key 就<b>一定</b>加密——服务端没启用时注册直接
     * 失败并指出是它没配，绝不静默退回明文（那等于给中间人一个"抹掉加密"的开关）。
     */
    private void register() throws IOException {
        RegisterReq req = new RegisterReq();
        req.instanceId = instanceId;
        req.appId = config.getAppId();
        req.address = config.getAddress();
        req.version = VERSION;
        req.sdkType = config.getSdkType();
        String aesKey = config.getAesKey();
        if (aesKey == null || aesKey.isBlank()) {
            req.authScheme = RegisterCipher.SCHEME_PLAIN;
            req.password = config.getPassword();
        } else {
            // 配置写错（长度/非 hex）在这一刻就报，而不是解出一堆垃圾再报"口令错误"
            byte[] key = RegisterCipher.parseKey(aesKey);
            ChallengeResp ch = sealChallenge(req.appId);
            RegisterCipher.Sealed sealed = RegisterCipher.seal(key, req.appId, instanceId,
                    ch.challenge, config.getPassword(), transport.mapper());
            req.authScheme = RegisterCipher.SCHEME_AES_GCM;
            req.kid = sealed.kid();
            req.iv = sealed.iv();
            req.ciphertext = sealed.ciphertext();
            req.challenge = sealed.challenge();
        }
        JsonNode data = transport.request("/worker/register", req, null);
        RegisterResp resp = transport.mapper().convertValue(data, RegisterResp.class);
        if (resp == null || resp.token == null || resp.token.isEmpty()) {
            throw new IOException("register returned no token");
        }
        long ttl = resp.ttlSeconds > 0 ? resp.ttlSeconds * 1000L : tokenTtlMillis;
        this.tokenTtlMillis = ttl;
        this.tokenExpireAtMillis = System.currentTimeMillis() + ttl;
        this.token = resp.token;
        log.info("worker token issued: instanceId={} ttl={}s encrypted={}",
                instanceId, resp.ttlSeconds, RegisterCipher.SCHEME_AES_GCM.equals(req.authScheme));
    }

    /** 握手第一步：领一张一次性 challenge。服务端未启用加密时这一步就报错，线索在它自己的文案里。 */
    private ChallengeResp sealChallenge(String appId) throws IOException {
        JsonNode node = transport.request("/worker/challenge", new ChallengeReq(instanceId, appId), null);
        ChallengeResp resp = transport.mapper().convertValue(node, ChallengeResp.class);
        if (resp == null || resp.challenge == null || resp.challenge.isEmpty()) {
            throw new IOException("服务端返回了空 challenge（多半是它没启用注册加密，而本节点配了 aesKey）");
        }
        return resp;
    }

    /**
     * 剩余有效期不足 1/4 时主动重注册。放在心跳里最省事：既不会让节点在有效期边界上
     * 集体吃一次 401 再补注册，也不依赖任何服务端状态。注册失败不致命——真过期了还有
     * {@link #call} 的 401 自愈那条路兜底。
     */
    private void renewIfNeeded() {
        long left = tokenExpireAtMillis - System.currentTimeMillis();
        if (token == null || tokenExpireAtMillis == 0 || left > tokenTtlMillis / 4) {
            return;
        }
        synchronized (registerLock) {
            if (tokenExpireAtMillis - System.currentTimeMillis() > tokenTtlMillis / 4) {
                return; // 别的线程刚换过票
            }
            try {
                log.info("worker token nearing expiry (剩余 {}ms), re-registering", left);
                register();
            } catch (Exception e) {
                log.warn("worker proactive re-register failed: {}", e.toString());
            }
        }
    }

    /** 带 token 发一次数据面请求。 */
    private JsonNode call(String path, Tokenized req) throws IOException {
        return call(path, req, null);
    }

    /**
     * 带 token 发一次数据面请求；服务端 401（票过期、服务重启、项目改口令）时
     * 重新注册并用同一个请求体重放一次，业务侧感觉不到凭证轮换。
     *
     * @param onCall 请求已构造、尚未执行时的回调；fetch 用它记住 Call 以便 close() 掐断长轮询
     */
    private JsonNode call(String path, Tokenized req, Consumer<Call> onCall) throws IOException {
        String used = null;
        for (int attempt = 0; ; attempt++) {
            used = token;
            req.setToken(used);
            try {
                // transport 内部负责"这台没答上话就换下一台"，这里只管凭证
                return transport.request(path, req, onCall);
            } catch (ServerException e) {
                if (e.getCode() != 401 || attempt > 0) {
                    throw e;
                }
                log.warn("{}: worker token rejected ({}), re-registering", path, e.getMessage());
                synchronized (registerLock) {
                    // 已有别的线程换到新票就直接复用，避免一次 401 引发注册风暴
                    if (Objects.equals(used, token)) {
                        register();
                    }
                }
            }
        }
    }

    // ---------- 心跳 ----------

    /**
     * 心跳自排期：每跳之后按 {@code ±25%} 的抖动重新入队，而不是固定周期。
     * 与 Go SDK 的 {@code heartbeatLoop} 同语义。
     *
     * <p>必须有抖动：心跳是 worker 侧唯一高频写库的请求（每节点每跳 upsert 一行
     * worker_nodes）。整池节点同时冷启动后相位一致，每 {@code heartBeatSeconds} 秒
     * <b>齐射</b>一次；实测 200 节点在 sqlite 上把单条心跳写成到 237~338ms（服务端
     * SLOW SQL），而任务认领是同一条写通道上的事务，被排到 1 秒开外。相位打散之后
     * 齐射摊成匀速，同一场景的广播轮次从整轮 ~1s 回到 max 140ms 量级。
     */
    private void scheduleHeartbeat() {
        long intervalMs = config.getHeartBeatSeconds() * 1000L;
        long wait = intervalMs;
        if (intervalMs >= 2000) { // 间隔太短就别抖
            wait += (long) (Math.random() * (intervalMs / 2)) - intervalMs / 4;
        }
        heartbeatPool.schedule(() -> {
            heartbeatOnce();
            scheduleHeartbeat();
        }, wait, TimeUnit.MILLISECONDS);
    }

    private void heartbeatOnce() {
        renewIfNeeded();
        HeartbeatReq req = new HeartbeatReq();
        req.instanceId = instanceId;
        req.appId = config.getAppId();
        req.address = config.getAddress();
        req.version = VERSION;
        req.sdkType = config.getSdkType();
        req.cpuLoad = readCpuLoad();
        req.cpuProcessors = Runtime.getRuntime().availableProcessors();
        req.memoryUsed = readMemoryUsedMb();
        req.memoryMax = readMemoryMaxMb();
        req.tag = config.getTag();
        // 运行时指标随心跳上报，控制台「节点监控」页据此显示进程 CPU/GC/线程/fd
        runtimeMetrics.fill(req, config.getMaxConcurrency() - slots.availablePermits());
        try {
            JsonNode data = call("/worker/heartbeat", req);
            // 满载时 fetch 循环不发包，心跳是停止指令的兜底通道
            if (data != null && data.has("cancel_task_ids")) {
                List<Long> ids = new ArrayList<>();
                for (JsonNode n : data.get("cancel_task_ids")) {
                    ids.add(n.asLong());
                }
                cancelTasks(ids);
            }
            // 集群路由提示同样只走心跳这一条通道（零额外 QPS；兼容协议是每 10s 单独问一次 /server/acquire）
            if (data != null && data.path("cluster").isObject()) {
                applyHint(transport.mapper().convertValue(data.get("cluster"), ClusterHint.class));
            }
        } catch (Exception e) {
            if (shutdownRequested) {
                return;
            }
            log.warn("heartbeat failed: {}", e.toString());
        }
    }

    /**
     * 处理服务端下发的集群路由提示，回答"worker 如何选择并确认 owner"（与 Go SDK 的 applyHint 逐条对齐）：
     *
     * <ol>
     *   <li>{@code servers} 一律并进已知地址集合——这是 worker 感知"集群后来又起了一个副本"的唯一来源；</li>
     *   <li>{@code owner} 为空 = 没有可用提示（单机部署、租约正在交接、或对方没配对外地址），保持现状；</li>
     *   <li>{@code owner} 就是当前地址 = 已确认，清零计数（这一步是"确认"：我们只信这个副本自己也
     *       认为 owner 是谁，而不是只看一次别人的转述）；</li>
     *   <li>{@code owner} 与当前地址不同：必须它已在已知集合内、且连续 {@link #OWNER_CONFIRM_BEATS}
     *       跳都指向它才迁移。单跳不动作，是为了不让一台还没刷到新租约的副本把 worker 甩来甩去；</li>
     *   <li>迁过去若连不上，{@link HttpTransport} 的故障转移会自动退回其它已知地址——提示不承重，
     *       所以这里不需要任何"回滚 owner"的状态。</li>
     * </ol>
     *
     * <p>只有心跳线程调用，故确认状态无需加锁；迁移只影响下一次拉取周期，
     * 但会主动放弃挂在旧地址上的那一次长轮询，不必等它自然到点。
     */
    private void applyHint(ClusterHint hint) {
        if (hint == null) {
            return;
        }
        if (hint.servers != null && !hint.servers.isEmpty()) {
            int added = transport.note(hint.servers);
            if (added > 0) {
                log.info("gojob worker: 从集群提示学到 {} 个新副本地址，当前集合={}", added, hint.servers);
            }
        }
        String owner = HttpTransport.normalizeAddress(hint.owner);
        if (owner.isEmpty() || owner.equals(transport.current())) {
            hintOwner = "";
            hintSeen = 0;
            return;
        }
        if (!owner.equals(hintOwner)) {
            hintOwner = owner; // 第一跳看到新 owner：只记下，不动作
            hintSeen = 1;
            return;
        }
        if (hintSeen + 1 < OWNER_CONFIRM_BEATS) {
            hintSeen++;
            log.info("gojob worker: 集群提示 owner={}（{}/{} 确认中，暂不迁移）", owner, hintSeen, OWNER_CONFIRM_BEATS);
            return;
        }
        if (!transport.jumpTo(owner)) {
            log.info("gojob worker: 集群提示 owner={} 还不在已知副本集合里，本次忽略（下一跳再确认）", owner);
            hintOwner = "";
            hintSeen = 0;
            return;
        }
        log.info("gojob worker: owner {} 已连续 {} 跳确认，长轮询迁过去", owner, OWNER_CONFIRM_BEATS);
        hintOwner = "";
        hintSeen = 0;
        abandonActiveFetch();
    }

    /**
     * 放弃挂在旧地址上的那次长轮询。没有可掐的（不在拉取中）就是空操作，
     * 所以从心跳线程调用是安全的；被放弃的那批任务由 server 侧 ReassignLostTasks 回收重派，
     * 属"至少一次 + 回报判重"，不会丢任务。
     */
    private void abandonActiveFetch() {
        Call call = activeFetchCall;
        if (call != null) {
            log.info("gojob worker: server 地址已变更，放弃挂在旧地址上的长轮询");
            call.cancel();
        }
    }

    /** 系统平均负载；平台不支持时（返回负值）报 0。 */
    private static double readCpuLoad() {
        double load = ManagementFactory.getOperatingSystemMXBean().getSystemLoadAverage();
        return load < 0 ? 0 : load;
    }

    /** JVM 已用堆内存，单位 MB。 */
    private static double readMemoryUsedMb() {
        Runtime rt = Runtime.getRuntime();
        long used = rt.totalMemory() - rt.freeMemory();
        return used / (1024.0 * 1024.0);
    }

    /** JVM 堆上限（-Xmx），单位 MB，对应兼容协议的 jvmMaxMemory。 */
    private static double readMemoryMaxMb() {
        return Runtime.getRuntime().maxMemory() / (1024.0 * 1024.0);
    }

    // ---------- 拉取 ----------

    private void fetchLoop() {
        while (!shutdownRequested) {
            if (!receiving.get()) { // 优雅下线：只等标志翻回 true 或进程退出，不再拉任务
                sleepQuietly(1000);
                continue;
            }
            int free = slots.availablePermits();
            if (free <= 0) { // 并发额度已满：等"有额度了"，1s 只是兜底
                try {
                    slotFree.poll(1, TimeUnit.SECONDS);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    return;
                }
                continue;
            }
            FetchTaskReq req = new FetchTaskReq();
            req.instanceId = instanceId;
            req.appId = config.getAppId();
            req.address = config.getAddress();
            req.maxTaskNum = free;
            req.waitSeconds = config.getFetchWaitSeconds();
            try {
                JsonNode data = call("/worker/fetchTask", req, c -> activeFetchCall = c);
                FetchTaskResp fetched = extractFetched(data);
                cancelTasks(fetched.cancelTaskIds);
                List<TaskDTO> tasks = fetched.tasks == null ? Collections.emptyList() : fetched.tasks;
                for (TaskDTO task : tasks) {
                    try {
                        slots.acquire(); // fetch 是唯一提交方，请求量=当时空闲额度，不会死锁
                    } catch (InterruptedException e) {
                        Thread.currentThread().interrupt();
                        return;
                    }
                    try {
                        taskPool.execute(() -> runTask(task));
                    } catch (RejectedExecutionException e) {
                        slots.release();
                        pokeSlotFree();
                        log.warn("worker draining, drop fetched task {} (server will reassign)", task.taskId);
                        return;
                    }
                }
            } catch (ServerException e) {
                // "no alive worker for app"：刚注册还没心跳生效，静默短退避
                if (e.getCode() == 500 && e.getMessage() != null && e.getMessage().contains("no alive worker")) {
                    log.debug("fetchTask: {} (waiting first heartbeat to land)", e.getMessage());
                    sleepQuietly(1000);
                } else {
                    log.warn("fetchTask failed: {}", e.toString());
                    sleepQuietly(2000);
                }
            } catch (IOException e) {
                if (shutdownRequested) {
                    return; // close() 取消在途请求属正常退出
                }
                log.warn("fetchTask failed: {}", e.toString());
                sleepQuietly(2000);
            } finally {
                activeFetchCall = null;
            }
        }
    }

    private FetchTaskResp extractFetched(JsonNode data) {
        FetchTaskResp empty = new FetchTaskResp();
        empty.tasks = Collections.emptyList();
        if (data == null || data.isNull() || data.isMissingNode()) {
            return empty;
        }
        FetchTaskResp resp;
        try {
            resp = transport.mapper().convertValue(data, FetchTaskResp.class);
        } catch (IllegalArgumentException e) {
            log.warn("fetchTask: unexpected data shape: {}", e.toString());
            return empty;
        }
        if (resp == null) {
            return empty;
        }
        if (resp.tasks == null) {
            resp.tasks = Collections.emptyList();
        }
        return resp;
    }

    // ---------- 多任务：派生子任务 / 取子任务结果 ----------

    /**
     * {@link TaskContext#map} 的实现体：把每个子任务编码成 JSON 发给 server。
     * 失败抛 {@link GoJobException}——派生没成功就不能让处理器以为"已经分出去了"。
     */
    List<String> mapTasks(TaskContext ctx, List<?> taskList, String taskName, int batchNo) {
        MapTaskReq req = new MapTaskReq();
        req.instanceId = instanceId;
        req.appId = config.getAppId();
        req.taskId = ctx.getTaskId();
        req.taskName = taskName;
        req.batchNo = batchNo;
        List<String> payloads = new ArrayList<>(taskList.size());
        for (int i = 0; i < taskList.size(); i++) {
            try {
                payloads.add(transport.mapper().writeValueAsString(taskList.get(i)));
            } catch (Exception e) {
                throw new GoJobException("第 " + i + " 个子任务无法序列化为 JSON: " + e.getMessage(), e);
            }
        }
        req.subTasks = payloads;
        try {
            JsonNode data = call("/worker/mapTask", req);
            MapTaskResp resp = transport.mapper().convertValue(data, MapTaskResp.class);
            if (resp == null || resp.taskKeys == null) {
                throw new GoJobException("mapTask 返回了空响应");
            }
            ctx.info("map 派生 {} 个子任务（taskName={}, 批次 {}）", resp.taskKeys.size(), taskName, batchNo);
            return resp.taskKeys;
        } catch (GoJobException e) {
            throw e;
        } catch (Exception e) {
            throw new GoJobException("map 失败: " + e.getMessage(), e);
        }
    }

    /** 分页取满本实例的子任务结果，交给 reduce。 */
    List<TaskResult> fetchTaskResults(TaskContext ctx) {
        List<TaskResult> out = new ArrayList<>();
        int offset = 0;
        try {
            for (; ; ) {
                TaskResultsReq req = new TaskResultsReq();
                req.instanceId = instanceId;
                req.appId = config.getAppId();
                req.taskId = ctx.getTaskId();
                req.offset = offset;
                req.limit = 500;
                JsonNode data = call("/worker/taskResults", req);
                TaskResultsResp resp = transport.mapper().convertValue(data, TaskResultsResp.class);
                if (resp == null || resp.results == null || resp.results.isEmpty()) {
                    return out;
                }
                for (TaskResultsResp.Row r : resp.results) {
                    out.add(new TaskResult(r.taskKey, r.taskName, r.success, r.result));
                }
                offset += resp.results.size();
                if (!resp.hasMore) {
                    return out;
                }
            }
        } catch (GoJobException e) {
            throw e;
        } catch (Exception e) {
            throw new GoJobException("取子任务结果失败: " + e.getMessage(), e);
        }
    }

    // ---------- 执行与回报 ----------

    private void runTask(TaskDTO task) {
        TaskLogger taskLogger = new TaskLogger(task, logReporter);
        runningThreads.put(task.taskId, Thread.currentThread());
        ProcessResult result;
        try {
            Processor processor = resolveProcessor(task.processor);
            if (stopRequested.contains(task.taskId)) {
                // 停止指令在开跑前就到：不执行处理器，直接按 STOPPED 收尾
                result = new ProcessResult(false, "cancelled before start");
            } else if ("EXTERNAL".equalsIgnoreCase(task.processorType)) {
                // 兼容协议里 EXTERNAL 表示去 JAR 动态容器按 容器ID#类名 加载，本 SDK 没有对应
                // 实现；说清楚比报"找不到处理器"好排查（沿用兼容协议的 fetch Processor failed 文案）
                result = new ProcessResult(false, "processorType=EXTERNAL is not supported by gojob worker"
                        + " (the compat protocol loads it from a JAR container as containerID#className);"
                        + " implement it in the worker and use BUILT_IN");
            } else if (processor == null) {
                result = new ProcessResult(false, "processor '" + task.processor
                        + "' not found: not registered and not loadable as class name");
            } else {
                TaskContext context = new TaskContext(
                        task.taskId, task.jobId, task.instanceId, task.jobParams, task.fixedParams,
                        task.attempts, task.retryTimes, taskLogger,
                        task.taskKey, task.taskName, task.executeType, task.subTaskContent, this);
                ProcessResult r;
                if (context.isLastTask()) {
                    // reduce 任务（兼容协议的 OMS_LAST_TASK）：本次执行的成败与结果由它说了算，
                    // 所以"部分子任务失败但可接受"这种容错只能写在 reduce 里。
                    if (!(processor instanceof MapReduceProcessor)) {
                        r = new ProcessResult(false, "processor '" + task.processor
                                + "' does not implement MapReduceProcessor, but this job is MAP_REDUCE");
                    } else {
                        r = ((MapReduceProcessor) processor).reduce(context, context.getSubTaskResults());
                    }
                } else {
                    r = processor.process(context);
                }
                if (r == null) {
                    result = new ProcessResult(false, "processor returned null ProcessResult");
                } else {
                    result = r;
                }
            }
        } catch (Throwable t) {
            result = new ProcessResult(false, t.getClass().getName() + ": " + t.getMessage());
            log.error("task {} (job {}) execution failed", task.taskId, task.jobId, t);
        } finally {
            runningThreads.remove(task.taskId);
        }
        boolean stopped = stopRequested.remove(task.taskId);
        try {
            // 与 Go SDK 对齐：失败写一条 ERROR；先保证日志送达（终态实例必可查），再回报结果
            if (stopped) {
                taskLogger.warn("task stopped by server request");
            } else if (!result.isSuccess()) {
                taskLogger.error("task failed: {}", result.getMessage());
            }
            taskLogger.flush(30_000);
            report(task, result, stopped);
        } finally {
            slots.release(); // 回报完成后再归还额度，保证 drain 语义
            pokeSlotFree();
        }
    }

    /**
     * 执行服务端夹带下来的停止指令：标记 + interrupt 执行线程。
     * 中断是协作式的——处理器需响应 InterruptedException（或自行轮询线程中断标志），
     * 否则会一直跑到自然结束；那种情况下回报仍按 STOPPED 记账。
     */
    private void cancelTasks(List<Long> taskIds) {
        if (taskIds == null || taskIds.isEmpty()) {
            return;
        }
        for (Long id : taskIds) {
            if (id == null) {
                continue;
            }
            // 服务端会把未 ack 的 cancel 一直夹带在响应里，重复下发是常态：只认第一次。
            // 否则每轮长轮询都会重新 interrupt 执行线程一次、再刷一行日志。
            if (!stopRequested.add(id)) {
                continue;
            }
            Thread t = runningThreads.get(id);
            if (t != null) {
                log.info("task {} stop requested by server, interrupting executor", id);
                t.interrupt();
            }
        }
    }

    /**
     * 服务端是否已对本 taskId 下过停止指令。{@link TaskContext#isStopped()} 读它——
     * 比线程中断标志可靠：处理器里 catch 掉 InterruptedException 会把标志清掉，
     * 而这个标记要到回报之后才移除。
     */
    boolean isStopRequested(long taskId) {
        return stopRequested.contains(taskId);
    }

    private void report(TaskDTO task, ProcessResult result, boolean stopped) {
        ReportTaskReq req = new ReportTaskReq();
        req.instanceId = instanceId;
        req.appId = config.getAppId();
        req.taskId = task.taskId;
        req.success = result.isSuccess() && !stopped;
        req.result = result.getMessage() == null ? "" : result.getMessage();
        if (stopped) {
            req.status = "STOPPED";
        } else if (result.isSuccess()) {
            req.status = "SUCCEED";
        } else {
            req.status = "FAILED";
            log.warn("task {} failed: {}", task.taskId, req.result);
        }
        for (int attempt = 1; attempt <= 3; attempt++) {
            try {
                call("/worker/report", req);
                return;
            } catch (Exception e) {
                log.warn("report task {} failed (attempt {}/3): {}", task.taskId, attempt, e.toString());
                sleepQuietly(2000);
            }
        }
        // 回报彻底失败：任务停在 RUNNING，Server 的超时回收（ReassignLostTasks）会重新入队，无需本地重试
        log.error("report task {} still failed after 3 attempts; rely on server reassign", task.taskId);
    }

    /** 归还额度后喊一声；队列容量 1，满了就合并成一次唤醒（唤醒是"该去看一眼"，不是计数）。 */
    private void pokeSlotFree() {
        slotFree.offer(new Object());
    }

    private static void sleepQuietly(long millis) {
        try {
            Thread.sleep(millis);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }
}
