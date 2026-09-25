package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/** /worker/reportLogs 请求体，对应 protocol.ReportLogsReq。 */
public class ReportLogsReq implements Tokenized {

    /** worker 实例 ID（与心跳一致），非任务实例。 */
    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    /** /worker/register 签发的短周期凭证；数据面不再携带项目口令。 */
    @JsonProperty("token")
    public String token;

    @JsonProperty("entries")
    public List<TaskLogEntry> entries;

    @Override
    public void setToken(String token) {
        this.token = token;
    }
}
