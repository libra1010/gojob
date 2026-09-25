package com.gojob.worker.spring;

import com.gojob.worker.GoJobWorker;
import com.gojob.worker.Processor;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.ObjectProvider;
import org.springframework.boot.autoconfigure.AutoConfiguration;
import org.springframework.boot.autoconfigure.condition.ConditionalOnClass;
import org.springframework.boot.autoconfigure.condition.ConditionalOnMissingBean;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.context.ApplicationContext;
import org.springframework.util.ClassUtils;
import org.springframework.context.annotation.Bean;
import org.springframework.core.annotation.AnnotatedElementUtils;

/**
 * gojob worker 的 Spring Boot 自动装配。
 *
 * <p>装配流程：
 * <ol>
 *   <li>按 {@link GoJobWorkerProperties}（前缀 {@code gojob.worker}）构建 core 的
 *       {@link GoJobWorker} 并 start；</li>
 *   <li>把容器内所有 {@link Processor} Bean 以其类全名注册（与 Server 端
 *       processor=Java 类全名 的分发语义一致，PowerJob 导入任务原样可用），
 *       走 Spring 单例而非 SDK 内部反射 new，业务处理器可直接依赖注入；</li>
 *   <li>带 {@link GoJobProcessor} 注解的 Bean 额外以显式 name 注册；</li>
 *   <li>容器关闭时调用 {@link GoJobWorker#close()}：先停止收单，
 *       等待运行中任务执行并回报完毕（drain）后退出，实现优雅下线。</li>
 * </ol>
 */
@AutoConfiguration
@ConditionalOnClass(GoJobWorker.class)
@ConditionalOnProperty(prefix = "gojob.worker", name = "enable", matchIfMissing = true)
@EnableConfigurationProperties(GoJobWorkerProperties.class)
public class GoJobWorkerAutoConfiguration {

    private static final Logger log = LoggerFactory.getLogger(GoJobWorkerAutoConfiguration.class);

    @Bean(destroyMethod = "close") // close() 幂等且会 drain 运行中任务，Spring 容器关闭时自动触发
    @ConditionalOnMissingBean
    public GoJobWorker goJobWorker(GoJobWorkerProperties properties, ApplicationContext context,
                                   ObjectProvider<Processor> processors) {
        GoJobWorker worker = new GoJobWorker(properties.toOpenConfig());
        processors.orderedStream().forEach(processor -> register(worker, processor));
        worker.start();
        log.info("gojob worker auto-configured: app={} (processors registered by FQCN)",
                properties.getAppId());
        return worker;
    }

    /** 类全名注册（去 CGLIB 代理取原始类）；带 @GoJobProcessor(name) 的再加一个显式名字。 */
    private static void register(GoJobWorker worker, Processor processor) {
        Class<?> userClass = ClassUtils.getUserClass(processor);
        worker.registerProcessor(userClass.getName(), processor);
        GoJobProcessor annotation = AnnotatedElementUtils.findMergedAnnotation(userClass, GoJobProcessor.class);
        if (annotation != null && !annotation.name().isEmpty()) {
            worker.registerProcessor(annotation.name(), processor);
        }
    }
}
