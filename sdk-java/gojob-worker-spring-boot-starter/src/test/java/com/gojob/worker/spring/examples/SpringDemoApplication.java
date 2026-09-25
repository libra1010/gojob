package com.gojob.worker.spring.examples;

import com.gojob.worker.ProcessResult;
import com.gojob.worker.Processor;
import com.gojob.worker.TaskContext;
import com.gojob.worker.spring.GoJobProcessor;

import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.stereotype.Service;

/**
 * Spring Boot 接入示例（仅编译级示例，不随 starter 发布，也不含运行测试）。
 *
 * <p>宿主应用只需：
 * <ol>
 *   <li>引入 gojob-worker-spring-boot-starter 依赖；</li>
 *   <li>application.yml 配置 {@code gojob.worker.server-address / app-id / password}；</li>
 *   <li>把业务处理器写成 Spring Bean（如下），Server 端 Job 的 processor 字段填
 *       该类的<b>类全名</b>
 *       {@code com.gojob.worker.spring.examples.SpringDemoProcessor$Hello}；
 *       或用 {@code @GoJobProcessor(name = "hello")} 再配一个短名。</li>
 * </ol>
 *
 * <p>处理器作为 Spring 单例注册（而非 SDK 反射实例化），因此可以直接构造器注入
 * 其他 Bean（Repository、DataSource 等）。
 */
@SpringBootApplication
public class SpringDemoApplication {

    /** 用法一：普通 @Service，按类全名分发（与 PowerJob 导入任务语义一致）。 */
    @Service
    public static class Hello implements Processor {

        @Override
        public ProcessResult process(TaskContext context) {
            return new ProcessResult(true, "hello from spring, taskId=" + context.getTaskId());
        }
    }

    /** 用法二：@GoJobProcessor 显式命名（自带 @Component 语义），processor 字段填 "greeting"。 */
    @GoJobProcessor(name = "greeting")
    public static class Greeting implements Processor {

        @Override
        public ProcessResult process(TaskContext context) {
            return new ProcessResult(true, "greeting done, instance=" + context.getInstanceId());
        }
    }
}
