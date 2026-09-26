package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * 注册握手第一步的请求，对应 protocol.ChallengeReq。
 *
 * <p>challenge 是一次性的：用过一次即作废，所以每个 worker 每次注册都要重新领一张。
 * 服务端未启用注册加密时这一步会明确报错（而不是悄悄发一张没用的票）。
 */
public class ChallengeReq {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    public ChallengeReq(String instanceId, String appId) {
        this.instanceId = instanceId;
        this.appId = appId;
    }
}
