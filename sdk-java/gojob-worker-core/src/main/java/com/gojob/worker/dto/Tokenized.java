package com.gojob.worker.dto;

/**
 * 需要携带 worker token 的数据面请求体。
 *
 * <p>token 由 {@code /worker/register} 用项目口令换取，服务端把它与 (app_id, instance_id)
 * 绑定；SDK 收到 401 时换新票后用同一个对象重放，因此需要统一的写入入口。
 */
public interface Tokenized {

    void setToken(String token);
}
