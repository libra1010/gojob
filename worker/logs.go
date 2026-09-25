// 日志上报通道：worker 本地缓冲 + 批量发送 + ack 后才出队（at-least-once），
// 配合 server 端 (task_id, seq) 唯一索引去重，实现"基本不丢不重"。
package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gojob/internal/protocol"
)

const (
	logBatchSize = 200   // 攒够即发
	logFlushMax  = 500   // 单次上报条数上限（与 server maxLogBatchEntries 协调）
	logQueueMax  = 50000 // 本地缓冲上限，超出丢最旧并计数（极端场景兜底）
)

var levelOrder = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3, "OFF": 4}

func levelAt(threshold, level string) bool {
	t, ok := levelOrder[threshold]
	if !ok {
		t = levelOrder["INFO"]
	}
	return levelOrder[level] >= t
}

type queuedLog struct {
	entry protocol.TaskLogEntry
	owner *taskLogger
}

// logSender 全局发送器：一条 goroutine 顺序消费队列，失败无限退避重试，
// 只有 server 返回 code=0 才把该批条目移出队列 —— 重试造成的重复由 server 幂等消化。
type logSender struct {
	w *Worker

	mu      sync.Mutex
	queue   []queuedLog
	dropped atomic.Int64
	notify  chan struct{}
}

func newLogSender(w *Worker) *logSender {
	return &logSender{
		w:      w,
		notify: make(chan struct{}, 1),
	}
}

func (s *logSender) enqueue(items ...queuedLog) {
	s.mu.Lock()
	if len(s.queue)+len(items) > logQueueMax {
		keep := logQueueMax - len(items)
		if keep < 0 {
			keep = 0
		}
		drop := s.queue[:len(s.queue)-keep]
		s.queue = s.queue[len(s.queue)-keep:]
		for _, q := range drop {
			q.owner.decrement()
		}
		s.dropped.Add(int64(len(drop)))
	}
	s.queue = append(s.queue, items...)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// run 主循环：入队达到批量阈值立即（去抖后）发送；零散日志最迟约 2 秒送达一次，
// 保证控制台近实时可见且请求量可控。
func (s *logSender) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
			if s.pending() < logBatchSize {
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond): // 去抖攒批
				}
			}
		case <-time.After(2 * time.Second):
		}
		for s.pending() > 0 {
			if !s.sendOnce(ctx) {
				break // 网络失败，条目留在队列，下轮重试
			}
		}
	}
}

func (s *logSender) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// sendOnce 认领并发送一批：先在锁内把批次从队列摘走，发送成功后本批才算 ack；
// 失败则整批放回队首等待重试（顺序不变），因此并发的 sendOnce/flush 不会重发同一条目。
func (s *logSender) sendOnce(ctx context.Context) bool {
	s.mu.Lock()
	n := min(len(s.queue), logFlushMax)
	if n == 0 {
		s.mu.Unlock()
		return true
	}
	batch := make([]queuedLog, n)
	copy(batch, s.queue)
	s.queue = s.queue[n:]
	s.mu.Unlock()

	entries := make([]protocol.TaskLogEntry, len(batch))
	for i, q := range batch {
		entries[i] = q.entry
	}
	req := &protocol.ReportLogsReq{
		InstanceId: s.w.instanceID, AppId: s.w.cfg.AppId,
		Entries: entries,
	}
	if _, err := call[int](s.w, ctx, "/worker/reportLogs", req); err != nil {
		log.Printf("reportLogs %d entries: %v (will retry)", len(entries), err)
		s.mu.Lock()
		restored := make([]queuedLog, 0, n+len(s.queue))
		restored = append(restored, batch...)
		restored = append(restored, s.queue...)
		s.queue = restored
		s.mu.Unlock()
		return false
	}
	for _, q := range batch {
		q.owner.decrement()
	}
	return true
}

// flush 阻塞至队列为空或 ctx 结束（网络恢复后重试直至送达）。
func (s *logSender) flush(ctx context.Context) {
	for {
		if s.pending() == 0 {
			return
		}
		if s.sendOnce(ctx) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ---------- taskLogger：单任务视角 ----------

type taskLogger struct {
	task      protocol.TaskDTO
	threshold string
	s         *logSender
	seq       atomic.Uint64
	pending   atomic.Int64
}

func newTaskLogger(s *logSender, task protocol.TaskDTO) *taskLogger {
	threshold := task.LogLevel
	if threshold == "" {
		threshold = "INFO"
	}
	return &taskLogger{task: task, threshold: threshold, s: s}
}

func (l *taskLogger) output(level, format string, args ...any) {
	// 处理器在 worker 执行链路之外被直接调用时（单测、业务方自己 new TaskContext）没有日志
	// 管道，此时丢日志比 panic 后被恢复成"processor panic"更合理。
	if l == nil {
		return
	}
	if !levelAt(l.threshold, level) {
		return
	}
	e := protocol.TaskLogEntry{
		TaskId: l.task.TaskId, InstanceId: l.task.InstanceId,
		Seq: l.seq.Add(1), Time: time.Now().UnixMilli(), Level: level,
		Content: fmt.Sprintf(format, args...),
	}
	l.pending.Add(1)
	l.s.enqueue(queuedLog{entry: e, owner: l})
}

func (l *taskLogger) decrement() { l.pending.Add(-1) }

// flush 等待本任务已产生的日志全部收到 server ack。
func (l *taskLogger) flush(ctx context.Context) {
	for l.pending.Load() > 0 {
		// 全局 flush 会把队列里所有条目（含本任务的）按序送达
		sent := l.s.sendOnce(ctx)
		if l.pending.Load() == 0 || sent {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// ---------- TaskContext 日志 API ----------

func (tc *TaskContext) Debugf(format string, args ...any) {
	tc.logger.output("DEBUG", format, args...)
}
func (tc *TaskContext) Infof(format string, args ...any) {
	tc.logger.output("INFO", format, args...)
}
func (tc *TaskContext) Warnf(format string, args ...any) {
	tc.logger.output("WARN", format, args...)
}
func (tc *TaskContext) Errorf(format string, args ...any) {
	tc.logger.output("ERROR", format, args...)
}
func (tc *TaskContext) Info(args ...any) {
	tc.logger.output("INFO", "%s", fmt.Sprint(args...))
}
