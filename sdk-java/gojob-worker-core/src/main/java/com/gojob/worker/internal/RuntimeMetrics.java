package com.gojob.worker.internal;

import com.gojob.worker.dto.HeartbeatReq;
import java.lang.management.GarbageCollectorMXBean;
import java.lang.management.ManagementFactory;
import java.lang.management.ThreadMXBean;

/**
 * JVM 侧运行时指标采集，填进心跳包供控制台「节点监控」展示。
 *
 * <p>与 Go SDK 的口径对齐：取不到的项一律留 0（页面显示"—"），不猜数。
 * 进程 CPU 占用用 {@code getProcessCpuTime} 的差值算，100 = 吃满一个核，
 * 这样两侧数字可以横向比较；RSS 在 JVM 里没有可移植取法，故不报。
 */
public final class RuntimeMetrics {

    private final com.sun.management.OperatingSystemMXBean os;
    private final ThreadMXBean threads;
    private long prevCpuNanos = -1;
    private long prevWallMillis = -1;

    public RuntimeMetrics() {
        this.os = probeHotSpot();
        this.threads = ManagementFactory.getThreadMXBean();
    }

    /** 非 HotSpot（如部分精简 JVM）拿不到这个类型，此时相关项整体报 0。 */
    private static com.sun.management.OperatingSystemMXBean probeHotSpot() {
        var bean = ManagementFactory.getOperatingSystemMXBean();
        return bean instanceof com.sun.management.OperatingSystemMXBean os ? os : null;
    }

    public void fill(HeartbeatReq req, int runningTasks) {
        long gcCount = 0;
        long gcMillis = 0;
        for (GarbageCollectorMXBean gc : ManagementFactory.getGarbageCollectorMXBeans()) {
            if (gc.getCollectionCount() >= 0) {
                gcCount += gc.getCollectionCount();
            }
            if (gc.getCollectionTime() >= 0) {
                gcMillis += gc.getCollectionTime();
            }
        }
        req.gcCount = gcCount;
        req.gcPauseMs = gcMillis;
        req.threads = threads.getThreadCount();
        req.runningTasks = runningTasks;
        if (os == null) {
            return;
        }
        if (os instanceof com.sun.management.UnixOperatingSystemMXBean unix) {
            long fds = unix.getOpenFileDescriptorCount();
            req.openFd = fds > 0 ? (int) Math.min(fds, Integer.MAX_VALUE) : 0;
        }
        long cpuNanos = os.getProcessCpuTime();
        long wall = System.currentTimeMillis();
        if (cpuNanos > 0 && prevCpuNanos > 0 && wall > prevWallMillis) {
            double percent = (cpuNanos - prevCpuNanos) / 1e6 / (wall - prevWallMillis) * 100;
            req.cpuPercent = Math.round(Math.max(percent, 0) * 10) / 10.0;
        }
        prevCpuNanos = cpuNanos;
        prevWallMillis = wall;
    }
}
