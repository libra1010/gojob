package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonInclude;
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

    /**
     * 口令的送达方式：{@code plain}（默认）| {@code aes-gcm}。
     * 配了 aesKey 时走后者，此时 {@link #password} 必须为空——口令只存在于密文里。
     */
    @JsonProperty(value = "auth_scheme", required = false)
    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public String authScheme;

    /** 刚领到的一次性 challenge 原文。 */
    @JsonProperty("challenge")
    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public String challenge;

    /** 用哪把 key（key 自身指纹）。 */
    @JsonProperty("kid")
    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public String kid;

    /** AES-GCM nonce，base64，12 字节。 */
    @JsonProperty("iv")
    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public String iv;

    /** base64，认证标签附在尾部（与 Go 侧 gcm.Seal 同构）。 */
    @JsonProperty("ciphertext")
    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public String ciphertext;
}
