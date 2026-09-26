package com.gojob.worker.examples;

import com.gojob.worker.ProcessResult;
import com.gojob.worker.Processor;
import com.gojob.worker.TaskContext;

/**
 * 类全名方式接入的处理器示例。
 *
 * <p>Server 端 Job 的 processor 字段（或兼容协议导入的 processorInfo）填
 * {@code com.gojob.worker.examples.DemoProcessor}，SDK 会 Class.forName + 无参构造
 * 实例化并缓存复用，因此实现类必须保持无状态（提供的实例为进程内单例）。
 */
public class DemoProcessor implements Processor {

    @Override
    public ProcessResult process(TaskContext context) throws Exception {
        context.info("DemoProcessor 开始执行，第 {} 次尝试", context.getAttempts());
        // job_params 是创建 Job 时配置的 JSON 字符串，可直接反序列化为业务参数对象
        DemoParams params = context.getJobParamsAs(DemoParams.class);
        context.info("参数解析成功：greeting={}，times={}", params.greeting, params.times);
        System.out.printf("DemoProcessor ran: greeting=%s, times=%d, attempts=%d/%d%n",
                params.greeting, params.times, context.getAttempts(),
                context.getAttempts() + context.getRetryTimes());
        return new ProcessResult(true, "demo ok");
    }
}
