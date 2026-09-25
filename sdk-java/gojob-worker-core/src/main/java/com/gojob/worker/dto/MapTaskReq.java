package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/** /worker/mapTask 请求体，对应 protocol.MapTaskReq（处理器运行期派生子任务）。 */
public class MapTaskReq implements Tokenized {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    @JsonProperty("token")
    public String token;

    /** 派生子任务的父任务 ID（本次正在执行的任务）。 */
    @JsonProperty("task_id")
    public long taskId;

    @JsonProperty("task_name")
    public String taskName;

    /** 本任务内第几次调 map；HTTP 重放时保持不变，服务端据此识别重放而不是再建一批。 */
    @JsonProperty("batch_no")
    public int batchNo;

    /** 每个子任务的 JSON 负载。 */
    @JsonProperty("sub_tasks")
    public List<String> subTasks;

    @Override
    public void setToken(String token) {
        this.token = token;
    }
}
