package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** 单个任务，对应 protocol.TaskDTO。 */
public class TaskDTO {

    @JsonProperty("task_id")
    public long taskId;

    @JsonProperty("job_id")
    public long jobId;

    @JsonProperty("instance_id")
    public long instanceId;

    @JsonProperty("app_id")
    public String appId;

    /** SDK 内注册名 或 Java 类全名（兼容协议导入的 processorInfo）。 */
    @JsonProperty("processor")
    public String processor;

    /**
     * 处理器类型（兼容协议 ProcessorType）：BUILT_IN | EXTERNAL，空按 BUILT_IN。
     * EXTERNAL 是兼容协议的 JAR 动态容器（容器ID#类名），本 SDK 会直接判失败。
     */
    @JsonProperty("processor_type")
    public String processorType;

    /** 创建 Job 时配置的静态参数，JSON 字符串，由处理器自行反序列化。 */
    @JsonProperty("job_params")
    public String jobParams;

    /**
     * 执行类型（兼容协议 ExecuteType）：STANDALONE|BROADCAST|MAP|MAP_REDUCE。
     * worker 据此判断当前任务能不能派生子任务。
     */
    @JsonProperty("execute_type")
    public String executeType;

    /** 层次任务号："0" 是根任务，子任务是 "0.1"，reduce 固定 "9999"。 */
    @JsonProperty("task_key")
    public String taskKey;

    /** 任务名：map() 的第二参；内部任务是 OMS_ROOT/LAST/BROADCAST_TASK。 */
    @JsonProperty("task_name")
    public String taskName;

    /** 子任务负载（JSON）。根任务与广播任务为空。 */
    @JsonProperty("sub_task_content")
    public String subTaskContent;

    /** runJob 触发的动态参数（兼容协议语义的 instanceParams），可能为空。 */
    @JsonProperty("fixed_params")
    public String fixedParams;

    /** 任务配置的日志级别 DEBUG|INFO|WARN|ERROR|OFF，空按 INFO 处理；worker 侧过滤。 */
    @JsonProperty("log_level")
    public String logLevel;

    /** 第几次尝试，从 1 开始。 */
    @JsonProperty("attempts")
    public int attempts;

    /** 剩余重试次数。 */
    @JsonProperty("retry_times")
    public int retryTimes;
}
