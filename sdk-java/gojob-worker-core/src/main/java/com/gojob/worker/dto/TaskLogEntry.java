package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** 单条任务日志，对应 protocol.TaskLogEntry；seq 任务内从 1 单调递增，server 按 (task_id,seq) 去重。 */
public class TaskLogEntry {

    @JsonProperty("task_id")
    public long taskId;

    @JsonProperty("instance_id")
    public long instanceId;

    @JsonProperty("seq")
    public long seq;

    /** 毫秒时间戳。 */
    @JsonProperty("time")
    public long time;

    @JsonProperty("level")
    public String level;

    @JsonProperty("content")
    public String content;
}
