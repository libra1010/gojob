package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * /worker/register 请求体：数据面唯一需要项目口令的入口，用它换取 token。
 *
 * <p>对应 server 端 protocol.RegisterReq。
 */
public class RegisterReq {

    @JsonProperty("instance_id")
    public String instanceId;

    @JsonProperty("app_id")
    public String appId;

    /** 项目口令明文，只在注册这一刻出现。 */
    @JsonProperty("password")
    public String password;

    @JsonProperty("address")
    public String address;

    @JsonProperty("version")
    public String version;

    /** SDK 类型：JAVA | JAVA_SPRINGBOOT | JAVA_QUARKUS。 */
    @JsonProperty("sdk_type")
    public String sdkType;
}
