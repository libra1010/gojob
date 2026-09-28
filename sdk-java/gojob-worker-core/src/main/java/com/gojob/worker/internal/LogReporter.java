package com.gojob.worker.internal;

import com.gojob.worker.dto.ReportLogsReq;
import com.gojob.worker.dto.TaskLogEntry;
import java.util.ArrayList;
import java.util.List;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * 任务日志上报器：本地 FIFO 缓冲 + 单发送线程批量 POST /worker/reportLogs。
 *
 * <p>可靠性与 Go SDK（worker/logs.go）完全对齐：
 * 只有 server 返回 code=0 的 ack 才把批次出队；失败整批放回队首无限重试
 * （at-least-once），条目携带任务内单调递增 seq，server 按 (task_id, seq)
 * 唯一索引去重，重发等效无重复。缓冲上限 50000，极端积压时丢最旧兜底。
 */
public final class LogReporter {

    private static final Logger log = LoggerFactory.getLogger(LogReporter.class);

    static final int BATCH_SIZE = 200;   // 攒够即发
    static final int FLUSH_MAX = 500;    // 单次上报条数上限（与 server maxLogBatchEntries 协调）
    static final int QUEUE_MAX = 50000;  // 本地缓冲上限，超出丢最旧

    private static final class Item {
        final TaskLogEntry entry;
        final TaskLogger owner;

        Item(TaskLogEntry entry, TaskLogger owner) {
            this.entry = entry;
            this.owner = owner;
        }
    }

    private final DataPlane plane;
    private final String instanceId;
    private final String appId;

    private final List<Item> queue = new ArrayList<>();
    private final Object lock = new Object();
    private volatile boolean running = true;
    private Thread sender;

    public LogReporter(DataPlane plane, String instanceId, String appId) {
        this.plane = plane;
        this.instanceId = instanceId;
        this.appId = appId;
    }

    public void start() {
        sender = new Thread(this::loop, "gojob-worker-logs");
        sender.setDaemon(true);
        sender.start();
    }

    void enqueue(TaskLogEntry entry, TaskLogger owner) {
        synchronized (lock) {
            int overflow = queue.size() + 1 - QUEUE_MAX;
            if (overflow > 0) {
                for (int i = 0; i < overflow; i++) {
                    queue.remove(0).owner.decrementPending();
                }
                log.warn("log buffer overflow, dropped oldest {} entries", overflow);
            }
            queue.add(new Item(entry, owner));
            if (queue.size() >= BATCH_SIZE) {
                lock.notifyAll();
            }
        }
    }

    private void loop() {
        while (running) {
            try {
                synchronized (lock) {
                    if (queue.size() < BATCH_SIZE) {
                        lock.wait(2000); // 零散日志最迟 2s 送达一次
                    }
                }
                while (running && sendOnce() > 0) {
                    // 有网络往返就继续排空
                }
            } catch (InterruptedException e) {
                return;
            } catch (Throwable t) {
                log.warn("log sender tick failed: {}", t.toString());
            }
        }
    }

    /**
     * 认领并发送一批（≤500 条）：锁内摘走批次，ack 成功才视为送达；
     * 失败整批放回队首。返回剩余条数。
     */
    int sendOnce() throws InterruptedException {
        List<Item> batch;
        synchronized (lock) {
            if (queue.isEmpty()) {
                return 0;
            }
            int n = Math.min(queue.size(), FLUSH_MAX);
            batch = new ArrayList<>(queue.subList(0, n));
            queue.subList(0, n).clear();
        }
        ReportLogsReq req = new ReportLogsReq();
        req.instanceId = instanceId;
        req.appId = appId;
        req.entries = new ArrayList<>(batch.size());
        for (Item item : batch) {
            req.entries.add(item.entry);
        }
        try {
            plane.call("/worker/reportLogs", req);
        } catch (Exception e) {
            log.warn("reportLogs {} entries failed: {} (will retry)", batch.size(), e.toString());
            synchronized (lock) {
                queue.addAll(0, batch);
                lock.notifyAll();
            }
            Thread.sleep(500);
            return -1;
        }
        for (Item item : batch) {
            item.owner.decrementPending();
        }
        synchronized (lock) {
            return queue.size();
        }
    }

    /** 阻塞至队列排空（全部拿到 server ack）或超时。 */
    public void flush(long timeoutMillis) {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        while (System.currentTimeMillis() < deadline) {
            synchronized (lock) {
                if (queue.isEmpty()) {
                    return;
                }
                lock.notifyAll(); // 唤醒发送线程加速处理
            }
            try {
                if (sendOnceSafely() < 0) {
                    Thread.sleep(300);
                }
            } catch (InterruptedException e) {
                return;
            }
        }
    }

    private int sendOnceSafely() throws InterruptedException {
        try {
            return sendOnce();
        } catch (InterruptedException e) {
            throw e;
        } catch (Exception e) {
            return -1;
        }
    }

    /** 停止发送线程；调用前应先 flush 或在 drain 后接受剩余丢弃。 */
    public void stop() {
        running = false;
        synchronized (lock) {
            lock.notifyAll();
        }
        if (sender != null) {
            sender.interrupt();
        }
    }
}
