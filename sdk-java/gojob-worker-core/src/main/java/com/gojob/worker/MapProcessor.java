package com.gojob.worker;

import java.util.List;

/**
 * MAP 型处理器：允许在运行期把一批子任务派发到集群执行（对标兼容协议的
 * tech.powerjob.worker.core.processor.sdk.MapProcessor）。
 *
 * <p>与兼容协议的唯一差别是派生入口：那边靠 ThreadLocal 拿到"当前任务"，所以写成
 * {@code map(list, name)}；gojob 把能力挂在上下文上，写成 {@code ctx.map(list, name)}，
 * 与 Go SDK 的 {@code ctx.Map} 保持同一种形状。子任务在谁身上执行由集群决定，
 * 因此负载必须是可跨进程解码的 JSON（兼容协议用可插拔序列化器，默认 JDK 序列化，
 * 那会让 Go worker 读不出 Java worker 派生的子任务）。
 *
 * <pre>{@code
 * public class Splitter implements MapProcessor {
 *     public ProcessResult process(TaskContext ctx) throws Exception {
 *         if (ctx.isRootTask()) {
 *             ctx.map(List.of("a.csv", "b.csv"), "DOWNLOAD");
 *             return new ProcessResult(true, "已派生 2 片");
 *         }
 *         String file = ctx.getSubTask(String.class);
 *         ...
 *     }
 * }
 * }</pre>
 */
public interface MapProcessor extends Processor {

    /** map 的推荐批量上限，超过时 worker 会打一条 WARN（兼容协议同值）。 */
    int RECOMMEND_BATCH_SIZE = 200;

    /** 调度器保留的任务名，用户不得拿来命名子任务。 */
    List<String> RESERVED_TASK_NAMES = List.of("OMS_ROOT_TASK", "OMS_LAST_TASK", "OMS_BROADCAST_TASK");
}
