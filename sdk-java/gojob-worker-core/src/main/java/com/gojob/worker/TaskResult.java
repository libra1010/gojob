package com.gojob.worker;

/**
 * 一个子任务的执行结果（对标 PowerJob 的 tech.powerjob.worker.core.processor.TaskResult）。
 * reduce 收到的就是这个列表。
 */
public final class TaskResult {

    private final String taskId;
    private final String taskName;
    private final boolean success;
    private final String result;

    public TaskResult(String taskId, String taskName, boolean success, String result) {
        this.taskId = taskId;
        this.taskName = taskName;
        this.success = success;
        this.result = result;
    }

    /** 层次任务号："0.1"、"0.2.3"…… 与 PowerJob 的 taskId 形态一致。 */
    public String getTaskId() {
        return taskId;
    }

    /** map() 时用户起的任务名。 */
    public String getTaskName() {
        return taskName;
    }

    public boolean isSuccess() {
        return success;
    }

    /** 子任务 ProcessResult 的 message；失败时是失败原因。 */
    public String getResult() {
        return result;
    }

    @Override
    public String toString() {
        return "TaskResult{" + taskId + ", " + taskName + ", success=" + success + ", result=" + result + '}';
    }
}
