package com.gojob.worker.internal;

import java.io.IOException;

/**
 * 非 200 的 HTTP 响应。
 *
 * <p>之所以要和普通 {@link IOException} 区分开：5xx 算"这台副本没答上话"（换一台可能就好了），
 * 4xx 是某个副本对这次请求的明确答复（换副本结果一样，不该触发故障转移）。
 */
public class HttpStatusException extends IOException {

    private final int status;

    public HttpStatusException(int status, String body) {
        super("server returned HTTP " + status + ": " + body);
        this.status = status;
    }

    public int status() {
        return status;
    }
}
