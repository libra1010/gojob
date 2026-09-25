package com.gojob.worker;

/** 处理器返回值：success + message（message 作为回报给 Server 的 result）。 */
public class ProcessResult {

    private final boolean success;
    private final String message;

    public ProcessResult(boolean success) {
        this(success, null);
    }

    public ProcessResult(boolean success, String message) {
        this.success = success;
        this.message = message;
    }

    public boolean isSuccess() {
        return success;
    }

    public String getMessage() {
        return message;
    }
}
