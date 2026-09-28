package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** /worker/heartbeat 请求体。字段与 server 端 protocol.HeartbeatReq（WorkerInfo 内联 + token）一一对应。 */
public class HeartbeatReq implements Tokenized {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    @JsonProperty("address")
    public String address;

    @JsonProperty("cpu_load")
    public double cpuLoad;

    /** 可用 CPU 核数，server 端健康分 = (内存上限-已用)*2 + (核数-负载)。 */
    @JsonProperty("cpu_processors")
    public int cpuProcessors;

    @JsonProperty("memory_used")
    public double memoryUsed;

    /** JVM 可堆上限（-Xmx），MB。 */
    @JsonProperty("memory_max")
    public double memoryMax;

    /** 机器标签，供 SPECIFY 分发策略匹配。 */
    @JsonProperty("tag")
    public String tag;

    @JsonProperty("version")
    public String version;

    /** SDK 类型：JAVA | JAVA_SPRINGBOOT | JAVA_QUARKUS，控制台统计页展示。 */
    @JsonProperty("sdk_type")
    public String sdkType;

    /** /worker/register 签发的短周期凭证；数据面不再携带项目口令。 */
    @JsonProperty("token")
    public String token;

    /** 本进程 CPU 占用，100 = 吃满一个核；取不到报 0。 */
    @JsonProperty("cpu_percent")
    public double cpuPercent;

    /** 启动以来 GC 次数合计与累计暂停毫秒（各收集器求和）。 */
    @JsonProperty("gc_count")
    public long gcCount;

    @JsonProperty("gc_pause_ms")
    public double gcPauseMs;

    /** 进程常驻物理内存；JVM 没有可移植的取法，恒为 0（页面显示"—"）。 */
    @JsonProperty("rss_mb")
    public double rssMb;

    @JsonProperty("threads")
    public int threads;

    /** 打开的 fd 数（含 socket）；非 unix 平台为 0。 */
    @JsonProperty("open_fds")
    public int openFd;

    /** 正在执行的任务数 = 已占用的并发额度。 */
    @JsonProperty("running_tasks")
    public int runningTasks;

    @Override
    public void setToken(String token) {
        this.token = token;
    }
}
