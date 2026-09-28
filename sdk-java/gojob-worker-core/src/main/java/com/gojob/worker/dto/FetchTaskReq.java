package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** /worker/fetchTask 请求体，对应 protocol.FetchTaskReq。长轮询拉取任务。 */
public class FetchTaskReq implements Tokenized {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    /** /worker/register 签发的短周期凭证；数据面不再携带项目口令。 */
    @JsonProperty("token")
    public String token;

    @JsonProperty("address")
    public String address;

    /** 本次最多拉取任务数，取 worker 当前空闲执行额度。 */
    @JsonProperty("max_task_num")
    public int maxTaskNum;

    /** 服务端最大阻塞秒数（服务端上限 120）。 */
    @JsonProperty("wait_seconds")
    public int waitSeconds;

    @Override
    public void setToken(String token) {
        this.token = token;
    }
}
