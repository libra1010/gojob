package com.gojob.worker;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.gojob.worker.internal.TaskLogger;
import java.util.List;

/** 传给 {@link Processor} 的执行上下文（对标 PowerJob JobContext 的子集）。 */
public final class TaskContext {

    private static final ObjectMapper PARAM_MAPPER =
            new ObjectMapper().disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES);

    private final long taskId;
    private final long jobId;
    private final long instanceId;
    private final String jobParams;
    private final String instanceParams;
    private final int attempts;
    private final int retryTimes;
    private final TaskLogger logger;

    // ---------- 多任务形态（BROADCAST / MAP / MAP_REDUCE） ----------

    /** 层次任务号："0" 根任务、"0.3" 第三片子任务、"9999" reduce 任务。 */
    private final String taskKey;
    /** 任务名：map() 的第二参；内部任务为 OMS_ROOT/LAST/BROADCAST_TASK。 */
    private final String taskName;
    /** 执行类型，决定 {@link #map} 是否可用。 */
    private final String executeType;
    /** 本子任务的负载原文（JSON）；根任务为空。 */
    private final String subTaskContent;
    /** 回到 worker 才能调数据面（派生子任务、取子任务结果）。 */
    private final GoJobWorker worker;
    /** 本任务内第几次调 map；HTTP 重放时不变，服务端据此识别重放。 */
    private int mapBatch;

    TaskContext(long taskId, long jobId, long instanceId, String jobParams, String instanceParams,
                int attempts, int retryTimes, TaskLogger logger) {
        this(taskId, jobId, instanceId, jobParams, instanceParams, attempts, retryTimes, logger,
                "0", "OMS_ROOT_TASK", "STANDALONE", null, null);
    }

    TaskContext(long taskId, long jobId, long instanceId, String jobParams, String instanceParams,
                int attempts, int retryTimes, TaskLogger logger, String taskKey, String taskName,
                String executeType, String subTaskContent, GoJobWorker worker) {
        this.taskId = taskId;
        this.jobId = jobId;
        this.instanceId = instanceId;
        this.jobParams = jobParams;
        this.instanceParams = instanceParams;
        this.attempts = attempts;
        this.retryTimes = retryTimes;
        this.logger = logger;
        this.taskKey = taskKey;
        this.taskName = taskName;
        this.executeType = executeType;
        this.subTaskContent = subTaskContent;
        this.worker = worker;
    }

    /** 层次任务号（PowerJob 的 TaskContext#getTaskId 是字符串形态，这里同名同形）。 */
    public String getTaskKey() {
        return taskKey;
    }

    /** map() 时起的任务名；根任务与内部任务是 OMS_* 保留名。 */
    public String getTaskName() {
        return taskName;
    }

    /** 本实例的执行类型。 */
    public String getExecuteType() {
        return executeType;
    }

    /** 是否为实例的根任务：MAP 型处理器的标准写法是"根任务算分片并 map 出去，子任务分支干真正的活"。 */
    public boolean isRootTask() {
        return taskKey == null || taskKey.isEmpty() || "0".equals(taskKey);
    }

    /** 是否为 reduce 任务（PowerJob 的 OMS_LAST_TASK）。 */
    public boolean isLastTask() {
        return "OMS_LAST_TASK".equals(taskName);
    }

    // ---------- 停止检查（三种写法任选其一） ----------

    /**
     * 服务端是否已要求停止本任务。三种写法都可以，越往下越省事：
     * <ol>
     *   <li>{@code Thread.currentThread().isInterrupted()} —— 原生，但 {@code catch} 掉
     *       {@code InterruptedException} 后中断标志会被 JVM 清掉，之后的检查全部失效；</li>
     *   <li>{@link #isStopped()} —— 读 worker 侧的停止标记，不受"中断被吞"影响；处理器把活
     *       派给自己的线程池时，把它当参数传下去比传中断标志可靠得多；</li>
     *   <li>{@link #checkStopped()} —— 循环开头调一次，已停止就直接抛出调用栈。</li>
     * </ol>
     * 走哪条都行，业务要做的只是<b>尽快返回</b>：SDK 按停止意图记账，回报 {@code STOPPED}
     * （不算失败、不触发重试）。
     */
    public boolean isStopped() {
        return Thread.currentThread().isInterrupted()
                || (worker != null && worker.isStopRequested(taskId));
    }

    /** 已停止则抛 {@link TaskStoppedException}。适合放在批处理/分页循环的开头。 */
    public void checkStopped() {
        if (isStopped()) {
            throw new TaskStoppedException("task " + taskId + " stopped by server request");
        }
    }

    /** {@link #checkStopped()} 抛出的非受检异常；冒泡出 {@code process()} 时 SDK 按 STOPPED 回报。 */
    public static class TaskStoppedException extends RuntimeException {
        public TaskStoppedException(String message) {
            super(message);
        }
    }

    /** 把本子任务的负载反序列化为目标类型；根任务没有负载，抛 {@link GoJobException}。 */
    public <T> T getSubTask(Class<T> clazz) {
        if (subTaskContent == null || subTaskContent.isEmpty()) {
            throw new GoJobException("本任务没有子任务负载（根任务或广播任务的这一份）");
        }
        try {
            return PARAM_MAPPER.readValue(subTaskContent, clazz);
        } catch (Exception e) {
            throw new GoJobException(
                    "failed to parse sub_task as " + clazz.getName() + ": " + e.getMessage(), e);
        }
    }

    /**
     * 派生子任务并交给集群执行（对标 PowerJob 的 MapProcessor.map）。
     *
     * <p>每个元素单独 JSON 编码，因此负载必须是可跨进程解码的普通数据——我们的 worker 池是
     * Go/Java 混布的，PowerJob 那套 JDK 序列化在这里读不出来。
     *
     * @return 服务端为这批子任务分配的任务号
     * @throws GoJobException 执行类型不允许 map、任务名是保留名、或服务端拒绝
     */
    public List<String> map(List<?> taskList, String taskName) {
        if (worker == null) {
            throw new GoJobException("该 TaskContext 不属于任何 worker 进程，无法派生子任务");
        }
        if (taskList == null || taskList.isEmpty()) {
            throw new GoJobException("map 的子任务列表不能为空");
        }
        if (!"MAP".equals(executeType) && !"MAP_REDUCE".equals(executeType)) {
            throw new GoJobException("执行类型 " + executeType + " 不允许派生子任务（把任务配成 MAP 或 MAP_REDUCE）");
        }
        if (MapProcessor.RESERVED_TASK_NAMES.contains(taskName)) {
            throw new GoJobException("taskName " + taskName + " 是调度器保留名");
        }
        if (taskList.size() > MapProcessor.RECOMMEND_BATCH_SIZE) {
            logger.warn("单次 map {} 个子任务偏多，建议分批（网络与队列压力）", taskList.size());
        }
        mapBatch++;
        return worker.mapTasks(this, taskList, taskName, mapBatch);
    }

    /** 本实例全部子任务的执行结果（不含根任务与 reduce 自身），worker 已分页取满。 */
    public List<TaskResult> getSubTaskResults() {
        if (worker == null) {
            throw new GoJobException("该 TaskContext 不属于任何 worker 进程");
        }
        return worker.fetchTaskResults(this);
    }

    // ---------- 任务执行日志（上传到 Server，控制台"任务日志"页可查） ----------
    // {} 占位符风格：ctx.info("处理订单 {}", orderId);

    public void debug(String pattern, Object... args) {
        logger.debug(pattern, args);
    }

    public void info(String pattern, Object... args) {
        logger.info(pattern, args);
    }

    public void warn(String pattern, Object... args) {
        logger.warn(pattern, args);
    }

    public void error(String pattern, Object... args) {
        logger.error(pattern, args);
    }

    public long getTaskId() {
        return taskId;
    }

    public long getJobId() {
        return jobId;
    }

    public long getInstanceId() {
        return instanceId;
    }

    /** Job 配置的静态参数原文（JSON 字符串），可能为 null/空串。 */
    public String getJobParams() {
        return jobParams;
    }

    /** runJob 触发时携带的动态参数原文（对标 PowerJob instanceParams），可能为 null/空串。 */
    public String getInstanceParams() {
        return instanceParams;
    }

    /** 把 instance_params 反序列化为目标类型；失败抛 {@link GoJobException}。 */
    public <T> T getInstanceParamsAs(Class<T> clazz) {
        if (instanceParams == null || instanceParams.isEmpty()) {
            throw new GoJobException("instance_params is empty, cannot deserialize to " + clazz.getName());
        }
        try {
            return PARAM_MAPPER.readValue(instanceParams, clazz);
        } catch (Exception e) {
            throw new GoJobException(
                    "failed to parse instance_params as " + clazz.getName() + ": " + e.getMessage(), e);
        }
    }

    /** 第几次尝试，从 1 开始。 */
    public int getAttempts() {
        return attempts;
    }

    /** 剩余重试次数。 */
    public int getRetryTimes() {
        return retryTimes;
    }

    /** 把 job_params 反序列化为目标类型；失败抛 {@link GoJobException}。 */
    public <T> T getJobParamsAs(Class<T> clazz) {
        if (jobParams == null || jobParams.isEmpty()) {
            throw new GoJobException("job_params is empty, cannot deserialize to " + clazz.getName());
        }
        try {
            return PARAM_MAPPER.readValue(jobParams, clazz);
        } catch (Exception e) {
            throw new GoJobException(
                    "failed to parse job_params as " + clazz.getName() + ": " + e.getMessage(), e);
        }
    }
}
