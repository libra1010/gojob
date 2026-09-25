package com.gojob.example.quarkus;

import com.gojob.worker.GoJobWorker;
import com.gojob.worker.OpenConfig;

import io.quarkus.runtime.ShutdownEvent;
import io.quarkus.runtime.StartupEvent;

import jakarta.enterprise.context.ApplicationScoped;
import jakarta.enterprise.event.Observes;
import jakarta.inject.Inject;

import org.eclipse.microprofile.config.inject.ConfigProperty;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Quarkus 接入方式：用 CDI 生命周期事件驱动 gojob worker。
 *
 * <ul>
 *   <li>{@code StartupEvent}（应用启动完成）→ 构造 {@link GoJobWorker}、
 *       按<b>类全名</b>注册容器里的 {@link HelloProcessor}（Server 端 processor 字段
 *       填 {@code com.gojob.example.quarkus.HelloProcessor} 即路由到此 bean，
 *       走的是 Spring/CDI 单例而不是 SDK 反射 new，业务依赖照常注入）、start；</li>
 *   <li>{@code ShutdownEvent}（应用停止）→ {@link GoJobWorker#close()}：
 *       自动执行优雅下线（停止收单 → 等运行中任务跑完并回报 → 释放资源）。</li>
 * </ul>
 *
 * <p>配置项走 MicroProfile Config（application.properties / 环境变量），
 * 见本模块 {@code src/main/resources/application.properties}。
 */
@ApplicationScoped
public class GoJobWorkerLifecycle {

    private static final Logger log = LoggerFactory.getLogger(GoJobWorkerLifecycle.class);

    @ConfigProperty(name = "gojob.server-address", defaultValue = "http://127.0.0.1:7700")
    String serverAddress;

    @ConfigProperty(name = "gojob.app-id", defaultValue = "demo")
    String appId;

    @ConfigProperty(name = "gojob.password", defaultValue = "demo123")
    String password;

    @ConfigProperty(name = "gojob.max-concurrency", defaultValue = "8")
    int maxConcurrency;

    @Inject
    HelloProcessor helloProcessor;

    private volatile GoJobWorker worker;

    /** 应用启动完成后：建 worker、注册处理器、开始拉任务。 */
    void onStart(@Observes StartupEvent event) {
        OpenConfig config = new OpenConfig()
                .setServerAddress(serverAddress)
                .setAppId(appId)
                .setPassword(password)
                .setMaxConcurrency(maxConcurrency)
                .setSdkType("JAVA_QUARKUS");

        worker = new GoJobWorker(config);

        // 按类全名注册 CDI bean：与 Server 端 processor=Java 类全名 的分发语义对齐
        worker.registerProcessor(HelloProcessor.class.getName(), helloProcessor);
        // 也可以注册短名：worker.registerProcessor("hello", helloProcessor);

        worker.start();
        log.info("gojob worker started: app={} server={}", appId, serverAddress);
    }

    /**
     * 应用停止前：优雅下线。close() 内部先 stopReceivingNewTasks()（不再拉新任务），
     * 再无限期等待在跑任务执行并回报完毕（drain），最后释放 HTTP 资源。
     */
    void onStop(@Observes ShutdownEvent event) {
        GoJobWorker current = worker;
        if (current != null) {
            log.info("gojob worker draining (stop receiving + wait running tasks)...");
            // 若想先停收单、观察一段时间再退出，可分步：
            //   current.stopReceivingNewTasks();  ... current.close();
            current.close();
        }
    }
}
