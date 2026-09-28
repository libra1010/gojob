// 停止指令与"开跑前就不执行"的短路：Go SDK 必须与 Java SDK 的 cancelled before start 同语义。
package worker

import (
	"context"
	"sync/atomic"
	"testing"

	"gojob/internal/protocol"
)

// cancelBeforeStart 复现"停止指令在处理器拿到任务之前就到了"这个现场：
// cancelTasks 在生产路径上做的就是 stoppedTasks.Store + cancel()，
// 所以这里预置标记 + 传一条已取消的 ctx，等价于那一瞬间的状态。
func cancelBeforeStart(t *testing.T, w *Worker, taskID uint64) context.Context {
	t.Helper()
	w.stoppedTasks.Store(taskID, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func lastReport(t *testing.T, f *mrFake) protocol.ReportTaskReq {
	t.Helper()
	if len(f.reports) != 1 {
		t.Fatalf("回报 %d 条，want 1：%+v", len(f.reports), f.reports)
	}
	return f.reports[0]
}

// 已经被判停止的任务不许再把业务副作用跑一遍。
func TestStopBeforeStartSkipsProcessor(t *testing.T) {
	var ran atomic.Bool
	f := &mrFake{}
	w := newMRWorker(t, f)
	Register("stopSkip", funcProc{fn: func(*TaskContext) error {
		ran.Store(true)
		return nil
	}})
	t.Cleanup(func() {
		procMu.Lock()
		delete(procs, "stopSkip")
		procMu.Unlock()
	})

	task := taskOf(protocol.ExecuteStandalone, protocol.RootTaskKey, protocol.RootTaskName, "")
	task.Processor = "stopSkip"
	w.execute(cancelBeforeStart(t, w, task.TaskId), task)
	w.running.Wait()

	if ran.Load() {
		t.Fatal("停止指令已在开跑前到达，处理器仍然被执行了——业务副作用会白做一遍")
	}
	r := lastReport(t, f)
	if r.Status != protocol.TaskStopped || r.Success {
		t.Fatalf("回报 = %+v, want STOPPED/success=false", r)
	}
	// 结果文本与 Java SDK 逐字一致，控制台结果列两边可对
	if r.Result != "cancelled before start" {
		t.Fatalf("result = %q, want cancelled before start", r.Result)
	}
}

// 关单（drain/进程退出）时 ctx 也会取消：同样不许起新任务，但这次不是用户停的，按失败回报。
func TestShutdownBeforeStartReportsFailed(t *testing.T) {
	var ran atomic.Bool
	f := &mrFake{}
	w := newMRWorker(t, f)
	Register("shutdownSkip", funcProc{fn: func(*TaskContext) error {
		ran.Store(true)
		return nil
	}})
	t.Cleanup(func() {
		procMu.Lock()
		delete(procs, "shutdownSkip")
		procMu.Unlock()
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 没有 stoppedTasks 标记：这是 worker 自己在下线，不是服务端停止
	task := taskOf(protocol.ExecuteStandalone, protocol.RootTaskKey, protocol.RootTaskName, "")
	task.Processor = "shutdownSkip"
	w.execute(ctx, task)
	w.running.Wait()

	if ran.Load() {
		t.Fatal("进程在下线，不该再开新任务")
	}
	r := lastReport(t, f)
	if r.Status != "" || r.Success {
		t.Fatalf("回报 = %+v, want 旧 SDK 口径的 FAILED（status 空 + success=false）", r)
	}
}

// 回归闸：处理器被取消后自己返回错误时，结果文本仍走通用文案，
// 不能被短路那句 "cancelled before start" 顶掉。
func TestStoppedAfterStartKeepsGenericResult(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	Register("cancelInside", funcProc{fn: func(ctx *TaskContext) error {
		// 已经开跑了：短路不该生效，走原来的"返回错误 + 记 STOPPED"路径
		return context.Canceled
	}})
	t.Cleanup(func() {
		procMu.Lock()
		delete(procs, "cancelInside")
		procMu.Unlock()
	})

	task := taskOf(protocol.ExecuteStandalone, protocol.RootTaskKey, protocol.RootTaskName, "")
	task.Processor = "cancelInside"
	w.stoppedTasks.Store(task.TaskId, true)
	w.execute(t.Context(), task)
	w.running.Wait()

	r := lastReport(t, f)
	if r.Status != protocol.TaskStopped {
		t.Fatalf("status = %q, want STOPPED", r.Status)
	}
	if r.Result != "stopped by server request" {
		t.Fatalf("result = %q, want stopped by server request", r.Result)
	}
}
