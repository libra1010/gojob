package com.gojob.worker.internal;

import com.gojob.worker.dto.TaskDTO;
import com.gojob.worker.dto.TaskLogEntry;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

/** 单任务视角的日志器：分配任务内单调递增 seq、按 log_level 过滤、跟踪未 ack 条数。 */
public final class TaskLogger {

    private final TaskDTO task;
    private final LogReporter reporter;
    private final int threshold;
    private final AtomicLong seq = new AtomicLong();
    private final AtomicInteger pending = new AtomicInteger();

    public TaskLogger(TaskDTO task, LogReporter reporter) {
        this.task = task;
        this.reporter = reporter;
        this.threshold = orderOf(task.logLevel == null || task.logLevel.isEmpty() ? "INFO" : task.logLevel);
    }

    private static int orderOf(String level) {
        switch (level.toUpperCase()) {
            case "DEBUG": return 0;
            case "INFO": return 1;
            case "WARN": return 2;
            case "ERROR": return 3;
            case "OFF": return 4;
            default: return 1;
        }
    }

    public void debug(String pattern, Object... args) {
        output("DEBUG", pattern, args);
    }

    public void info(String pattern, Object... args) {
        output("INFO", pattern, args);
    }

    public void warn(String pattern, Object... args) {
        output("WARN", pattern, args);
    }

    public void error(String pattern, Object... args) {
        output("ERROR", pattern, args);
    }

    public void output(String level, String pattern, Object... args) {
        if (orderOf(level) < threshold) {
            return;
        }
        TaskLogEntry entry = new TaskLogEntry();
        entry.taskId = task.taskId;
        entry.instanceId = task.instanceId;
        entry.seq = seq.incrementAndGet();
        entry.time = System.currentTimeMillis();
        entry.level = level;
        entry.content = format(pattern, args);
        pending.incrementAndGet();
        reporter.enqueue(entry, this);
    }

    /** SLF4J 风格 {} 占位替换。 */
    static String format(String pattern, Object... args) {
        if (pattern == null || args == null || args.length == 0) {
            return String.valueOf(pattern);
        }
        StringBuilder sb = new StringBuilder(pattern.length() + 32);
        int argIdx = 0;
        for (int i = 0; i < pattern.length(); i++) {
            char ch = pattern.charAt(i);
            if (ch == '{' && i + 1 < pattern.length() && pattern.charAt(i + 1) == '}' && argIdx < args.length) {
                sb.append(args[argIdx++]);
                i++;
            } else {
                sb.append(ch);
            }
        }
        return sb.toString();
    }

    void decrementPending() {
        pending.decrementAndGet();
    }

    /** 等待本任务已产生的日志全部拿到 server ack（任务结束先 flush 日志再回报结果）。 */
    public void flush(long timeoutMillis) {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        while (pending.get() > 0 && System.currentTimeMillis() < deadline) {
            reporter.flush(Math.min(2000, deadline - System.currentTimeMillis()));
            try {
                Thread.sleep(100);
            } catch (InterruptedException e) {
                return;
            }
        }
    }
}
