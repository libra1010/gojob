package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/** /worker/fetchTask 响应 data 字段，对应 protocol.FetchTaskResp。 */
public class FetchTaskResp {

    @JsonProperty("tasks")
    public List<TaskDTO> tasks;

    /** 下行指令：本 worker 名下被请求停止、尚未 ack 的任务 ID（pull 模型唯一的反向通道）。 */
    @JsonProperty("cancel_task_ids")
    public List<Long> cancelTaskIds;
}
