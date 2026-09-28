package com.gojob.worker;

/** 业务处理器接口（对标兼容协议的 BasicProcessor）。 */
@FunctionalInterface
public interface Processor {

    /**
     * 执行任务。抛出异常或返回 success=false 均按失败回报；
     * 失败后若 Server 侧还有剩余重试次数，会自动重新入队，SDK 不做本地重试。
     */
    ProcessResult process(TaskContext context) throws Exception;
}
