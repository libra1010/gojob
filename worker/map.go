// 多任务执行类型（BROADCAST / MAP / MAP_REDUCE）的 SDK 侧接口。
//
// 与 PowerJob 的对应关系：ctx.Map 对 MapProcessor.map(List, taskName)，
// ctx.SubTask 对 TaskContext.getSubTask()，ctx.IsRootTask 对 isRootTask()，
// Reducer 对 MapReduceProcessor.reduce(TaskContext, List<TaskResult>)。
// 差别在于 PowerJob 靠 ThreadLocal + 同机 TaskTracker 拿到这些能力，
// gojob 把句柄显式挂在 TaskContext 上、经 server 数据面中转——worker 之间不通信是我们的前提。
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gojob/internal/protocol"
)

// 单次 map 的子任务数上限，与服务端 dispatch.maxMapBatch 同值。
// 客户端先挡一道，是为了让超限在调用方的返回值里看得见，而不是等一次 HTTP 失败。
const maxMapBatch = 5000

// TaskResult 一个子任务的执行结果（PowerJob 的 TaskResult：taskId + success + result）。
type TaskResult struct {
	TaskKey  string // 层次 ID，如 "0.3"
	TaskName string // map() 时用户起的名字
	Success  bool
	Result   string // 该子任务 SetResult 的文本，失败时是错误原因
}

// Reducer 由 MAP_REDUCE 的处理器额外实现。
//
// 实例的执行类型选成 MAP_REDUCE 后，server 会在整棵任务树排空时补一个 reduce 任务
// （PowerJob 的 OMS_LAST_TASK），worker 认出它就调 Reduce 而不是 Process。
// 没实现这个接口却把任务配成 MAP_REDUCE，会得到一条明确的失败回报而不是静默跳过。
//
// Reduce 的返回值就是实例的最终结论：子任务部分失败时可以在这里自己兜住，
// 这正是 MAP_REDUCE 与 MAP 唯一的语义差别（MAP 是"任一子任务失败即实例失败"）。
type Reducer interface {
	Reduce(ctx *TaskContext, results []TaskResult) error
}

// SetResult 留下本任务的结果文本：非 MAP_REDUCE 型会成为实例结果（写进 job_instances.result），
// 在 MAP 型里则会出现在 reduce 收到的 results 中。Process 返回 error 时本方法的内容被错误原因覆盖。
func (c *TaskContext) SetResult(s string) { c.result = s }

// IsRootTask 是否为实例的根任务。MAP 型处理器的标准写法就是 PowerJob 那个形状：
// 根任务里算出分片、ctx.Map 派生出去，子任务分支里干真正的活。
func (c *TaskContext) IsRootTask() bool {
	return c.TaskKey == "" || c.TaskKey == protocol.RootTaskKey
}

// IsLastTask 是否为 reduce 任务。业务方一般不需要判它——worker 已经按 Reducer 接口分派过了。
func (c *TaskContext) IsLastTask() bool { return c.TaskName == protocol.LastTaskName }

// SubTask 把本子任务的负载解码到 v（指针）。
//
// PowerJob 用 worker 侧可插拔序列化器（默认 JDK 序列化），gojob 固定 JSON：
// 我们的 worker 池是 Go/Java 混布的，JDK 序列化的字节 Go 侧根本读不出来。
// 根任务没有负载，调用会返回错误而不是 panic。
func (c *TaskContext) SubTask(v any) error {
	if c.subTaskContent == "" {
		return errors.New("worker: 本任务没有子任务负载（根任务或广播的 preProcess 阶段）")
	}
	return json.Unmarshal([]byte(c.subTaskContent), v)
}

// Map 派生子任务并交给集群执行，返回 server 分配的任务序号列表（层次 ID）。
//
// taskList 的每个元素单独 JSON 编码成一份子任务负载；子任务在谁身上执行由集群决定
// （PowerJob 同语义：map 出去的任务会散到各 worker），所以负载必须是可跨进程解码的 JSON，
// 不能引用本进程的内存对象。
//
// 只有 MAP / MAP_REDUCE 型实例可以调用（STANDALONE 里调等于配置写错了，直接报错比静默忽略好查）。
// 子任务里再调 Map 是允许的，PowerJob 的多级 Map 就是这个用法。
func (c *TaskContext) Map(taskList []any, taskName string) ([]string, error) {
	if c.worker == nil {
		return nil, errors.New("worker: TaskContext 不属于任何 worker 进程，无法 map")
	}
	if len(taskList) == 0 {
		return nil, errors.New("worker: map 的子任务列表不能为空")
	}
	if len(taskList) > maxMapBatch {
		return nil, fmt.Errorf("worker: 单次 map 最多 %d 个子任务，本次 %d", maxMapBatch, len(taskList))
	}
	if !protocol.CanMap(c.ExecuteType) {
		return nil, fmt.Errorf("worker: 执行类型 %s 不允许派生子任务（把任务配成 MAP 或 MAP_REDUCE）", c.ExecuteType)
	}
	switch taskName {
	case protocol.RootTaskName, protocol.LastTaskName, protocol.BroadcastTaskName:
		return nil, fmt.Errorf("worker: taskName %q 是调度器保留名", taskName)
	}
	payloads := make([]string, 0, len(taskList))
	for i, t := range taskList {
		b, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("worker: 第 %d 个子任务无法序列化为 JSON: %w", i, err)
		}
		payloads = append(payloads, string(b))
	}
	// 批次号在重试间保持不变：HTTP 层重放同一个请求时，服务端靠 (任务, 批次) 认出是重放，
	// 回给 ta 已建好的任务而不是再建一批，避免"一次超时=双份执行"。
	c.mapBatch++
	req := &protocol.MapTaskReq{
		InstanceId: c.worker.instanceID, AppId: c.worker.cfg.AppId,
		TaskId: c.TaskID, TaskName: taskName, BatchNo: c.mapBatch, SubTasks: payloads,
	}
	resp, err := c.worker.mapTasks(c.Context, req)
	if err != nil {
		return nil, err
	}
	c.Infof("[SYSTEM] map 派生 %d 个子任务（taskName=%s，批次 %d）", len(resp.TaskKeys), taskName, c.mapBatch)
	return resp.TaskKeys, nil
}

// mapTasks 带一次 401 自愈（token 过期时重新注册再重放），与其余数据面调用同构。
func (w *Worker) mapTasks(ctx context.Context, req *protocol.MapTaskReq) (*protocol.MapTaskResp, error) {
	resp, err := call[protocol.MapTaskResp](w, ctx, "/worker/mapTask", req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("worker: mapTask 无响应体")
	}
	return &resp.Data, nil
}

// SubTaskResults 取本实例全部子任务的执行结果（不含根任务与 reduce 自身）。
//
// Reduce 被调用前 worker 已经用它取满一次并作为参数传进去了；单独暴露是为了让
// MAP 型处理器也能自查兄弟任务的结论（PowerJob 里等价操作是在 reduce 之前查本机表）。
func (c *TaskContext) SubTaskResults() ([]TaskResult, error) {
	if c.worker == nil {
		return nil, errors.New("worker: TaskContext 不属于任何 worker 进程")
	}
	var (
		out []TaskResult
		off int
	)
	for {
		if err := c.Context.Err(); err != nil {
			return nil, err
		}
		resp, err := c.worker.taskResults(c.Context, &protocol.TaskResultsReq{
			InstanceId: c.worker.instanceID, AppId: c.worker.cfg.AppId,
			TaskId: c.TaskID, Offset: off, Limit: 500,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range resp.Results {
			out = append(out, TaskResult{TaskKey: r.TaskKey, TaskName: r.TaskName,
				Success: r.Success, Result: r.Result})
		}
		off += len(resp.Results)
		if !resp.HasMore || len(resp.Results) == 0 {
			return out, nil
		}
	}
}

func (w *Worker) taskResults(ctx context.Context, req *protocol.TaskResultsReq) (*protocol.TaskResultsResp, error) {
	resp, err := call[protocol.TaskResultsResp](w, ctx, "/worker/taskResults", req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("worker: taskResults 无响应体")
	}
	return &resp.Data, nil
}
