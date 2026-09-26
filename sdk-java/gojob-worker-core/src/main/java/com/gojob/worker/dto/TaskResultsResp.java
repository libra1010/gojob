package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/**
 * /worker/taskResults 的请求与响应（reduce 取子任务结果，分页）。
 *
 * <p>分页是必需的：兼容协议的 LAST_TASK 从本机表一次性读全部结果，我们如果把这些结果
 * 塞进拉取响应，一个几万子任务的实例会先在 HTTP 响应里把自己撑爆。
 */
public class TaskResultsResp {

    public static class Row {

        @JsonProperty("task_key")
        public String taskKey;

        @JsonProperty("task_name")
        public String taskName;

        @JsonProperty("success")
        public boolean success;

        @JsonProperty("result")
        public String result;
    }

    @JsonProperty("results")
    public List<Row> results;

    /** 符合条件的总条数。 */
    @JsonProperty("total")
    public int total;

    @JsonProperty("has_more")
    public boolean hasMore;
}
