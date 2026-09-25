package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** /worker/report 请求体，对应 protocol.ReportTaskReq。 */
public class ReportTaskReq implements Tokenized {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    /** /worker/register 签发的短周期凭证；数据面不再携带项目口令。 */
    @JsonProperty("token")
    public String token;

    @JsonProperty("task_id")
    public long taskId;

    @JsonProperty("success")
    public boolean success;

    /** 终态：SUCCEED / FAILED / STOPPED；留空由 server 按 success 推导。 */
    @JsonProperty("status")
    public String status;

    /** 处理器返回值或错误信息。 */
    @JsonProperty("result")
    public String result;

    @Override
    public void setToken(String token) {
        this.token = token;
    }
}
