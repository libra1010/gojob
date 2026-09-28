package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/** /worker/challenge 响应体，对应 protocol.ChallengeResp。 */
public class ChallengeResp {

    /** 一次性凭证原文，注册时原样回传，并被封进密文里参与校验。 */
    @JsonProperty("challenge")
    public String challenge;

    /** 有效期秒数。 */
    @JsonProperty("expires_in")
    public int expiresIn;

    /** 服务端当前接受的注册方式，让"两侧只配了一半"这类错配在客户端就能看出原因。 */
    @JsonProperty("schemes")
    public List<String> schemes;
}
