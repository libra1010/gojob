package com.gojob.worker.internal;

import java.io.IOException;

/** Server 返回业务错误（HTTP 200 但信封 code != 0）。 */
public class ServerException extends IOException {

    private final int code;

    public ServerException(int code, String message) {
        super("server error(" + code + "): " + message);
        this.code = code;
    }

    public int getCode() {
        return code;
    }
}
