package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** /worker/taskResults 请求体，对应 protocol.TaskResultsReq。 */
public class TaskResultsReq implements Tokenized {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    @JsonProperty("token")
    public String token;

    /** 必须是 reduce 任务自身的 ID：服务端据此校验调用者就是正在执行该 reduce 任务的 worker。 */
    @JsonProperty("task_id")
    public long taskId;

    @JsonProperty("offset")
    public int offset;

    @JsonProperty("limit")
    public int limit;

    @Override
    public void setToken(String token) {
        this.token = token;
    }
}
