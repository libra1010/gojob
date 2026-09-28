package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/** /worker/mapTask 响应体。 */
public class MapTaskResp {

    /** 服务端为这批子任务分配的层次任务号（如 0.1、0.2）。 */
    @JsonProperty("task_keys")
    public List<String> taskKeys;

    /** 本次真正新建了几条；重放同一批时为 0。 */
    @JsonProperty("created")
    public int created;
}
