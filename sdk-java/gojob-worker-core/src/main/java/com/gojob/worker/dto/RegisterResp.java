package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/** /worker/register 响应体，对应 protocol.RegisterResp。 */
public class RegisterResp {

    @JsonProperty("token")
    public String token;

    /** 滑动有效期秒数：每次成功的数据面调用都会续期。 */
    @JsonProperty("ttl_seconds")
    public int ttlSeconds;
}
