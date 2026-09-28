package com.gojob.example.quarkus;

import com.gojob.worker.ProcessResult;
import com.gojob.worker.Processor;
import com.gojob.worker.TaskContext;

import jakarta.enterprise.context.ApplicationScoped;
import jakarta.inject.Inject;

/**
 * Quarkus/CDI 风格处理器示例：就是一个普通 bean，可注入任意业务依赖。
 *
 * <p>Server 端 Job 的 processor 字段填本类类全名
 * {@code com.gojob.example.quarkus.HelloProcessor}（与兼容协议导入任务的
 * processorInfo=类全名 语义一致）。
 */
@ApplicationScoped
public class HelloProcessor implements Processor {

    /** CDI 注入示例：处理器内部可以使用任何业务 bean。 */
    @Inject
    GreetingService greetingService;

    @Override
    public ProcessResult process(TaskContext context) {
        String result = greetingService.greet(context.getJobParams());
        return new ProcessResult(true, result + " (taskId=" + context.getTaskId() + ")");
    }
}
