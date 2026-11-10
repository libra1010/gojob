// 一实例多任务（BROADCAST / MAP / MAP_REDUCE）的收敛与派生逻辑。
//
// 为什么放在单独的文件里：dispatch.go 管的是"一条任务怎么从队列到 worker 再回来"，
// 本文件管的是"很多条任务怎么合成一个实例的结论"。前者是 pull 模型的机械结构，
// 后者是兼容协议语义里唯一需要重新设计的一部分——兼容目标把这部分逻辑放在
// worker 侧的 TaskTracker（子任务写本机 task_info 表、server 完全不知情），
// gojob 的 worker 之间不通信，所以这棵树必须长在库里。
package dispatch

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// 派生规模上限。兼容协议靠 worker 本地表没有显式上限（只有内存与线程池），
// gojob 的子任务要进 DB 并参与每次认领，必须有硬上限，否则一次 map(100万个) 就是
// 一次自伤式拒绝服务。超限直接判 map 失败，比让库卡死好查。
const (
	maxMapBatch        = 5000  // 单次 map() 的子任务数
	maxInstanceTasks   = 50000 // 单实例任务行数（含各级子任务）
	maxResultLength    = 8096  // 结果文本上限，与兼容协议的 worker 配置 maxResultLength 同值
	defaultResultsPage = 500
	maxResultsPage     = 2000
)

var (
	ErrNotMappable = errors.New("map is only allowed in MAP / MAP_REDUCE instances")
	ErrBatchTooBig = errors.New("too many sub-tasks in one map batch")
	ErrTreeTooBig  = errors.New("too many tasks in this instance")
	ErrReserved    = errors.New("task name is reserved by the scheduler")
)

// taskCounts 一个实例下任务树的形状，全部现算不缓存：
// 计数器落在 job_instances 上会被"派生子任务"与"崩溃在两步之间"打歪，
// 而这三列只用于展示与判定，一次带索引的聚合查询换掉整类漂移 bug 是划算的。
type taskCounts struct {
	Total   int
	Running int // WAITING + RUNNING，即尚未收敛的行数
	Failed  int
	Stopped int
}

func countTasks(tx *gorm.DB, instanceId uint64) (taskCounts, error) {
	var row struct {
		Total   int `gorm:"column:total"`
		Running int `gorm:"column:running"`
		Failed  int `gorm:"column:failed"`
		Stopped int `gorm:"column:stopped"`
	}
	// 条件聚合一次拿全，三库通用（CASE WHEN 是标准 SQL，不像 FILTER 只有 pg 认）
	err := tx.Model(&store.InstanceTask{}).
		Where("instance_id = ?", instanceId).
		Select(`COUNT(*) AS total,
			COALESCE(SUM(CASE WHEN status IN (?,?) THEN 1 ELSE 0 END), 0) AS running,
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS stopped`,
			protocol.TaskWaiting, protocol.TaskRunning, protocol.TaskFailed, protocol.TaskStopped).
		Scan(&row).Error
	if err != nil {
		return taskCounts{}, err
	}
	return taskCounts{Total: row.Total, Running: row.Running, Failed: row.Failed, Stopped: row.Stopped}, nil
}

// syncInstance 把现算出来的形状写回 job_instances（控制台的任务进度列读这几列）。
func syncInstance(tx *gorm.DB, instanceId uint64, c taskCounts, now time.Time) error {
	return tx.Model(&store.JobInstance{}).Where("id = ?", instanceId).
		Updates(map[string]any{
			"running_times":   c.Running,
			"total_task_num":  c.Total,
			"failed_task_num": c.Failed,
			"modify_time":     now,
		}).Error
}

// finishInstance 在"某个任务刚进入终态"之后重算实例形状并判断整棵任务树是否收敛。
// 返回非空表示本次调用把实例推到了终态（依赖触发只在"本次刚成功"时生效，不能重复触发）。
//
// 收敛条件是"没有未终态的行"，不是兼容协议里那句 unfinishedNum<=0 —— 同一件事的两种写法：
// 它数的是本机 task_info 表，我们数的是 instance_tasks 里这个实例的行。
func finishInstance(tx *gorm.DB, task *store.InstanceTask, outcome string, now time.Time) (string, error) {
	var inst store.JobInstance
	if err := tx.First(&inst, task.InstanceId).Error; err != nil {
		return "", err
	}
	c, err := countTasks(tx, inst.ID)
	if err != nil {
		return "", err
	}
	if err := syncInstance(tx, inst.ID, c, now); err != nil {
		return "", err
	}
	if c.Running > 0 {
		return "", nil // 树还没排空
	}
	// MAP_REDUCE 的最后一步此刻才生成 reduce 任务（兼容协议里的 OMS_LAST_TASK）：
	// 它出现后未终态数重新变成 1，实例继续运行，直到它也收敛。
	// 用户已经按下停止的实例不补这一步——那等于违背停止指令又起一个任务。
	if inst.ExecuteType == protocol.ExecuteMapReduce && !isStopping(&inst) {
		var last int64
		if err := tx.Model(&store.InstanceTask{}).
			Where("instance_id = ? AND task_name = ?", inst.ID, protocol.LastTaskName).
			Count(&last).Error; err != nil {
			return "", err
		}
		if last == 0 {
			if err := createReduceTask(tx, &inst, now); err != nil {
				return "", err
			}
			c, err = countTasks(tx, inst.ID)
			if err != nil {
				return "", err
			}
			if err := syncInstance(tx, inst.ID, c, now); err != nil {
				return "", err
			}
			return "", nil
		}
	}
	return settleInstance(tx, &inst, c, now)
}

func isStopping(inst *store.JobInstance) bool {
	return inst.Status == protocol.InstanceStopping || inst.Status == protocol.InstanceStopped
}

// settleInstance 任务树已排空，定实例终态与结果。
//
// 三条规则分别对应兼容协议的三处判定：
//   - 有被停止的行且实例正在停止中 → STOPPED（用户意图优先，兼容协议的 stopInstance
//     是先落库 STOPPED 再不可靠地通知 worker，gojob 沿用既有的"等协作退出"实现）；
//   - MAP_REDUCE → reduce 的返回值说了算，所以子任务部分失败时开发者可以自己兜住
//     （兼容协议同语义：实例状态取 LAST_TASK 的状态，result 取它的返回值）；
//   - 其余（STANDALONE/BROADCAST/MAP）→ 任一任务最终失败即实例失败
//     （兼容协议 CommonTaskTracker 那句 success = failedNum == 0）。
func settleInstance(tx *gorm.DB, inst *store.JobInstance, c taskCounts, now time.Time) (string, error) {
	status := protocol.InstanceSucceed
	result := ""
	switch {
	case c.Stopped > 0:
		// 有被停止的行就说明用户下过停止指令，这是用户意图，优先级最高。
		// 父任务重派而作废的后代不算在这里——那是 DISCARDED，一个独立的终态。
		status = protocol.InstanceStopped
	case inst.ExecuteType == protocol.ExecuteMapReduce:
		var last store.InstanceTask
		err := tx.Where("instance_id = ? AND task_name = ?", inst.ID, protocol.LastTaskName).
			First(&last).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// reduce 一行都没有：树在根任务失败后直接排空、且中途被停止过。
			// 没有 reduce 结论可用，退回"有失败即失败"，不能让实例凭空 SUCCEED。
			status = failedOrSucceed(c)
			result = rootResult(tx, inst.ID)
		} else if err != nil {
			return "", err
		} else {
			switch last.Status {
			case protocol.TaskSucceed:
				status = protocol.InstanceSucceed
			case protocol.TaskStopped, protocol.TaskDiscarded:
				status = protocol.InstanceStopped
			default:
				status = protocol.InstanceFailed
			}
			result = last.Result
		}
	case c.Failed > 0:
		status = protocol.InstanceFailed
		result = rootResult(tx, inst.ID)
	default:
		result = rootResult(tx, inst.ID)
	}
	if err := tx.Model(&store.JobInstance{}).Where("id = ?", inst.ID).
		Updates(map[string]any{"status": status, "result": result,
			"running_times": 0, "modify_time": now}).Error; err != nil {
		return "", err
	}
	return status, nil
}

func failedOrSucceed(c taskCounts) string {
	if c.Failed > 0 {
		return protocol.InstanceFailed
	}
	return protocol.InstanceSucceed
}

// rootResult 根任务的返回值作为实例结果（兼容协议把 root/last 任务的 ProcessResult.msg
// 作为 instanceInfo.result 上报，非 MAP_REDUCE 型就是根任务那一份）。
func rootResult(tx *gorm.DB, instanceId uint64) string {
	var root store.InstanceTask
	if err := tx.Where("instance_id = ? AND parent_task_id = ?", instanceId, 0).
		First(&root).Error; err != nil {
		return ""
	}
	return root.Result
}

// createReduceTask 生成 reduce 任务。宿主留空（任意在线节点可认领）是对兼容协议的
// 一处刻意偏离：它必须把 LAST_TASK 钉回 TaskTracker 本机，因为子任务结果在那台机器的
// 本地表里；gojob 的结果在库中，钉住只会多一个"那台机器挂了 reduce 永远没人跑"的失败模式。
func createReduceTask(tx *gorm.DB, inst *store.JobInstance, now time.Time) error {
	var root store.InstanceTask
	if err := tx.Where("instance_id = ? AND parent_task_id = ?", inst.ID, 0).
		First(&root).Error; err != nil {
		return fmt.Errorf("instance %d has no root task: %w", inst.ID, err)
	}
	last := store.InstanceTask{
		JobId: root.JobId, InstanceId: inst.ID, AppId: root.AppId,
		Processor: root.Processor, ProcessorType: root.ProcessorType,
		JobParams: root.JobParams, FixedParams: root.FixedParams,
		Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: root.MaxAttempts,
		LogLevel: root.LogLevel, ExecuteType: inst.ExecuteType,
		TaskKey: protocol.LastTaskKey, TaskName: protocol.LastTaskName,
		DispatchTime: now, CreateTime: now, ModifyTime: now,
	}
	return tx.Create(&last).Error
}

// discardDescendants 作废 taskID 的全部未终态后代。
//
// 为什么必须作废而不是留着：父任务重跑会再 map 一遍，旧子任务若还活着就会被执行第二次
// ——对一个"处理某笔订单"的子任务来说这是重复副作用，不是性能问题。
// 已终态的后代保留原状（它们是既成事实，也是排查依据），只有未收敛的被置 DISCARDED，
// 该状态不计入失败也不触发依赖下游。
func discardDescendants(tx *gorm.DB, taskID uint64, now time.Time) (int, error) {
	discarded := 0
	frontier := []uint64{taskID}
	for len(frontier) > 0 {
		var kids []store.InstanceTask
		err := tx.Where("parent_task_id IN ?", frontier).
			Find(&kids).Error
		if err != nil {
			return discarded, err
		}
		next := make([]uint64, 0, len(kids))
		for i := range kids {
			k := &kids[i]
			if protocol.IsTerminalTask(k.Status) {
				// 已出结果的不追溯，但它的子孙可能还在跑，继续往下走一层
				next = append(next, k.ID)
				continue
			}
			res := tx.Model(&store.InstanceTask{}).Where("id = ? AND status = ?", k.ID, k.Status).
				Updates(map[string]any{
					"status": protocol.TaskDiscarded, "result": "父任务重派，本任务作废",
					"stop_request_time": nil, "modify_time": now,
				})
			if res.Error != nil {
				return discarded, res.Error
			}
			if res.RowsAffected == 1 {
				discarded++
			}
			next = append(next, k.ID)
		}
		frontier = next
	}
	return discarded, nil
}

// MapTasks 处理处理器在运行期派生子任务的请求（兼容协议的 ProcessorMapTaskRequest）。
//
// 幂等靠 (parent_task_id, map_batch)：SDK 在 HTTP 层重放同一个批次时批次号不变，
// 服务端看到"这批已经建齐了"就直接把已有行的 task_key 回回去，不重复派生。
// 没有这一条，一次网络超时就是一次双倍执行。
func (d *Dispatcher) MapTasks(req *protocol.MapTaskReq) (*protocol.MapTaskResp, error) {
	if len(req.SubTasks) == 0 {
		return nil, errors.New("map: 子任务列表为空")
	}
	if len(req.SubTasks) > maxMapBatch {
		return nil, fmt.Errorf("%w: %d > %d（单次上限）", ErrBatchTooBig, len(req.SubTasks), maxMapBatch)
	}
	name := strings.TrimSpace(req.TaskName)
	if name == "" {
		return nil, errors.New("map: taskName 不能为空")
	}
	switch name {
	case protocol.RootTaskName, protocol.LastTaskName, protocol.BroadcastTaskName:
		return nil, fmt.Errorf("%w: %s", ErrReserved, name)
	}
	var resp protocol.MapTaskResp
	_, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		var t store.InstanceTask
		if e := tx.First(&t, req.TaskId).Error; e != nil {
			return struct{}{}, e
		}
		// 与 Report 同一道闸：只有正在执行该任务的 worker 能替它派生子任务，
		// 否则同项目任何持票节点都能凭空给别人的实例塞一批任务。
		if t.AppId != req.AppId || t.WorkerId != req.InstanceId {
			return struct{}{}, fmt.Errorf("map of task %d rejected: claimed by %q, requested by %q",
				t.ID, t.WorkerId, req.InstanceId)
		}
		if t.Status != protocol.TaskRunning {
			return struct{}{}, fmt.Errorf("task %d is %s, only a running task may map sub-tasks", t.ID, t.Status)
		}
		var inst store.JobInstance
		if e := tx.First(&inst, t.InstanceId).Error; e != nil {
			return struct{}{}, e
		}
		// 停止/已结束的实例不再接收新子任务：否则 map 出来的 WAITING 行会绕过
		// StopInstance 当时那一次批量收尾，等 worker 来认领时用户已经点过停止。
		if inst.Status != protocol.InstanceWaiting && inst.Status != protocol.InstanceRunning {
			return struct{}{}, fmt.Errorf("实例 %d 当前状态为 %s，拒绝派生新子任务", inst.ID, inst.Status)
		}
		if !protocol.CanMap(inst.ExecuteType) {
			return struct{}{}, fmt.Errorf("%w: 本实例执行类型为 %s", ErrNotMappable, inst.ExecuteType)
		}

		// 幂等键是 (父任务, 本次尝试, 批次号)。带上尝试次数：父任务重跑时会从批次 1 重新
		// 编号，如果不区分尝试，上一批里已成功的行会被当成"这批建过了"、新任务一条都不建。
		var existing []store.InstanceTask
		if e := tx.Where("parent_task_id = ? AND map_batch = ? AND map_attempt = ?",
			t.ID, req.BatchNo, t.Attempts).
			Order("id asc").Find(&existing).Error; e != nil {
			return struct{}{}, e
		}
		var all int64 // 含作废行的总数：只用于给新任务排 task_key，保证一个父任务下不出现重号
		if e := tx.Model(&store.InstanceTask{}).
			Where("parent_task_id = ?", t.ID).Count(&all).Error; e != nil {
			return struct{}{}, e
		}
		if len(existing) == len(req.SubTasks) && len(existing) > 0 {
			// 同一批次重放：内容不重新校验，兼容协议也是"以已落库的为准"
			resp.TaskKeys = make([]string, 0, len(existing))
			for i := range existing {
				resp.TaskKeys = append(resp.TaskKeys, existing[i].TaskKey)
			}
			return struct{}{}, nil
		}
		if len(existing) > 0 {
			// 一个事务里整批插入，理论上到不了这个分支；真到了说明数据被人工改过，
			// 与其猜半批怎么办，不如让调用方看到明确错误。
			return struct{}{}, fmt.Errorf("map batch %d of task %d is partially written (%d/%d)",
				req.BatchNo, t.ID, len(existing), len(req.SubTasks))
		}
		var live int64 // 实例已有行数（含作废的）：作废行仍占库、仍要计入规模上限
		if e := tx.Model(&store.InstanceTask{}).
			Where("instance_id = ?", inst.ID).Count(&live).Error; e != nil {
			return struct{}{}, e
		}
		if int(live)+len(req.SubTasks) > maxInstanceTasks {
			return struct{}{}, fmt.Errorf("%w: %d + %d > %d", ErrTreeTooBig, live, len(req.SubTasks), maxInstanceTasks)
		}
		now := store.Now()
		keys := make([]string, 0, len(req.SubTasks))
		rows := make([]store.InstanceTask, 0, len(req.SubTasks))
		for i, content := range req.SubTasks {
			key := fmt.Sprintf("%s.%d", t.TaskKey, int(all)+i+1)
			rows = append(rows, store.InstanceTask{
				JobId: t.JobId, InstanceId: t.InstanceId, AppId: t.AppId,
				Processor: t.Processor, ProcessorType: t.ProcessorType,
				JobParams: t.JobParams, FixedParams: t.FixedParams, LogLevel: t.LogLevel,
				ExecuteType:  inst.ExecuteType,
				ParentTaskId: t.ID, MapBatch: req.BatchNo, MapAttempt: t.Attempts,
				TaskKey: key, TaskName: name, SubTaskContent: content,
				Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: t.MaxAttempts,
				DispatchTime: now, CreateTime: now, ModifyTime: now,
			})
			keys = append(keys, key)
		}
		if e := tx.CreateInBatches(rows, 200).Error; e != nil {
			return struct{}{}, e
		}
		// 立刻把实例的任务数刷新：控制台的"任务进度"列读的是这三列，
		// 不刷新的话 map 出十万子任务的实例会一直显示 1/1，直到第一个子任务收尾。
		c, e := countTasks(tx, inst.ID)
		if e != nil {
			return struct{}{}, e
		}
		if e := syncInstance(tx, inst.ID, c, now); e != nil {
			return struct{}{}, e
		}
		resp.TaskKeys = keys
		resp.Created = len(keys)
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	if resp.Created > 0 {
		d.Notify() // 新任务入队，唤醒长轮询立刻投递
	}
	return &resp, nil
}

// TaskResults 分页取实例下所有子任务的执行结果，供 reduce 使用。
//
// 分页是给 reduce 的输入规模留后路：兼容协议的 LAST_TASK 从本机表一次性读全部结果，
// 我们如果把这些结果塞进 TaskDTO 随拉取响应下发，一个几万子任务的实例会先在 HTTP 响应里
// 把自己撑爆。SDK 侧照样是一次 Reduce(ctx, results) 调用，只是内部翻页取满。
func (d *Dispatcher) TaskResults(req *protocol.TaskResultsReq) (*protocol.TaskResultsResp, error) {
	limit := req.Limit
	switch {
	case limit <= 0:
		limit = defaultResultsPage
	case limit > maxResultsPage:
		limit = maxResultsPage
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	out := &protocol.TaskResultsResp{}
	_, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		var t store.InstanceTask
		if e := tx.First(&t, req.TaskId).Error; e != nil {
			return struct{}{}, e
		}
		if t.AppId != req.AppId || t.WorkerId != req.InstanceId {
			return struct{}{}, fmt.Errorf("task results of %d rejected: claimed by %q, requested by %q",
				t.ID, t.WorkerId, req.InstanceId)
		}
		if t.TaskName != protocol.LastTaskName {
			return struct{}{}, fmt.Errorf("task %d (%s) is not a reduce task", t.ID, t.TaskName)
		}
		// 口径与兼容协议的 getAllTaskResult 一致：除根与 reduce 自身之外的全部任务，
		// 也就是用户 map 出去的每一片（含多级 map 的中间层）。
		// 每次重新构造查询：gorm 的 *DB 复用会把上一条的条件带进下一条。
		q := func() *gorm.DB {
			return tx.Model(&store.InstanceTask{}).
				Where("instance_id = ? AND task_name NOT IN ?", t.InstanceId,
					[]string{protocol.RootTaskName, protocol.LastTaskName})
		}
		var total int64
		if e := q().Count(&total).Error; e != nil {
			return struct{}{}, e
		}
		out.Total = int(total)
		var rows []store.InstanceTask
		if e := q().Order("id asc").Limit(limit).Offset(req.Offset).Find(&rows).Error; e != nil {
			return struct{}{}, e
		}
		out.Results = make([]protocol.TaskResult, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out.Results = append(out.Results, protocol.TaskResult{
				TaskKey: r.TaskKey, TaskName: r.TaskName,
				Success: r.Status == protocol.TaskSucceed, Result: r.Result,
			})
		}
		out.HasMore = req.Offset+len(rows) < out.Total
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// truncResult 把处理器返回值裁到 maxResultLength，与兼容协议同口径：
// 结果会被写进实例行并回给控制台，不设上限等于让一个 println 撑爆数据库。
func truncResult(s string) string {
	if len(s) <= maxResultLength {
		return s
	}
	return s[:maxResultLength] + "...(结果已截断)"
}
