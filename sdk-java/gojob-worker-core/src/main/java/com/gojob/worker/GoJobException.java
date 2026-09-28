package com.gojob.worker;

/** SDK 内部异常（非受检），如 job_params 反序列化失败。 */
public class GoJobException extends RuntimeException {

    public GoJobException(String message) {
        super(message);
    }

    public GoJobException(String message, Throwable cause) {
        super(message, cause);
    }
}
