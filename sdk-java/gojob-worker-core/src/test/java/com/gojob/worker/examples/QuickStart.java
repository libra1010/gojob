package com.gojob.worker.examples;

import com.gojob.worker.GoJobWorker;
import com.gojob.worker.OpenConfig;
import com.gojob.worker.ProcessResult;

/**
 * 快速上手示例（main 方法，位于 test 源码目录，不随主 jar 发布）。
 *
 * <p>演示两种处理器接入方式与优雅下线：
 * <ol>
 *   <li>lambda + 手工注册名：Server 端 Job 的 processor 字段填 "hello"；</li>
 *   <li>类全名匹配：processor 字段填 "com.gojob.worker.examples.DemoProcessor"
 *       （兼容协议导入的任务 processorInfo 即类全名，SDK 自动 Class.forName 实例化）。</li>
 * </ol>
 *
 * <p>运行前确保 gojob Server 已启动，且已创建 app（app_id=demo / password=demo123），
 * 并建好指向上述 processor 的 Job。
 */
public final class QuickStart {

    public static void main(String[] args) {
        // 与 Go demo 一致的覆盖方式：GOJOB_WORKER_SERVER / GOJOB_WORKER_APP / GOJOB_WORKER_PASSWORD
        // 以及 GOJOB_WORKER_AES_KEY（配了则项目口令不再明文上线，须与服务端 GOJOB_WORKER_AES_KEYS 之一一致）
        OpenConfig config = new OpenConfig()
                .setServerAddress(env("GOJOB_WORKER_SERVER", "http://127.0.0.1:7800"))
                .setAppId(env("GOJOB_WORKER_APP", "demo"))
                .setPassword(env("GOJOB_WORKER_PASSWORD", "demo123"))
                .setAesKey(env("GOJOB_WORKER_AES_KEY", ""))
                .setMaxConcurrency(8); // 默认 16，可省略

        GoJobWorker worker = new GoJobWorker(config);

        // 方式一：注册名字 + lambda 处理器
        worker.registerProcessor("hello", context -> {
            // ctx.info/warn/error/debug：任务执行日志，SDK 可靠上传到 Server（控制台"任务日志"可查）
            context.info("hello 开始执行，taskId={}，jobId={}，第 {} 次尝试",
                    context.getTaskId(), context.getJobId(), context.getAttempts());
            System.out.printf("hello processor ran: taskId=%d, jobId=%d, attempts=%d, jobParams=%s%n",
                    context.getTaskId(), context.getJobId(), context.getAttempts(), context.getJobParams());
            context.info("hello 执行完成，jobParams={}", context.getJobParams());
            return new ProcessResult(true, "hello done");
        });

        // 方式二：类全名处理器（无需注册，Server 配 processor=com.gojob.worker.examples.DemoProcessor）

        worker.start();

        // Ctrl-C / kill 时优雅下线：停止收单并等待运行中任务执行完、回报完
        Runtime.getRuntime().addShutdownHook(new Thread(worker::close, "gojob-worker-shutdown-hook"));

        // 也可以随时主动只停收单（运行中任务继续），之后还能 resume：
        //   worker.stopReceivingNewTasks();
        //   ... // 观察 isReceiving() / 等任务排空
        //   worker.resumeReceivingNewTasks();

        worker.awaitTermination(); // 阻塞直到 close() 被调用
    }

    private static String env(String key, String def) {
        String v = System.getenv(key);
        return v == null || v.isEmpty() ? def : v;
    }

    private QuickStart() {
    }
}
