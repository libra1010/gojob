package com.gojob.worker;

import java.util.List;

/**
 * MAP_REDUCE 型处理器：在 {@link MapProcessor} 之上多一步汇总（对标兼容协议的
 * tech.powerjob.worker.core.processor.sdk.MapReduceProcessor）。
 *
 * <p>执行类型选成 MAP_REDUCE 后，server 会在整棵任务树排空时补一个 reduce 任务
 * （兼容协议的 OMS_LAST_TASK），worker 认出它就调 {@link #reduce} 而不是 {@link #process}。
 *
 * <p><b>reduce 的返回值就是本次执行的最终结论</b>：状态决定实例成败、message 决定实例结果，
 * 因此"部分子任务失败但整体可接受"这种容错只能写在这里。这也是 MAP_REDUCE 与 MAP 唯一的
 * 语义差别——MAP 型是"任一子任务最终失败即实例失败"，没有商量余地。
 *
 * <p>与兼容协议的一处实现差别：它必须把 reduce 钉回 TaskTracker 那台机器执行，因为子任务
 * 结果存在那台机器的本地表里；gojob 的结果在库中，所以 reduce 由任意空闲节点认领，
 * 少一个"那台机器挂了整次执行永远收不了尾"的失败模式。
 */
public interface MapReduceProcessor extends MapProcessor {

    /**
     * @param context     reduce 任务自己的上下文（{@code ctx.isLastTask()} 为 true）
     * @param taskResults 本实例所有子任务的结果，不含根任务与 reduce 自身；
     *                    顺序稳定（按派生先后），worker 已分页取满，不会截断
     */
    ProcessResult reduce(TaskContext context, List<TaskResult> taskResults);
}
