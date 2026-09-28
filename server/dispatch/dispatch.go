// Package dispatch 负责任务派发：Worker 长轮询拉取（调度执行分离的"执行层入口"）。
// M1 由 Worker 自行拉取任意节点空闲任务；工作节点存活校验在此完成。
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/cluster"
	"gojob/server/registry"
	"gojob/server/store"
)

var ErrNoWorker = errors.New("no alive worker for app")

// FinishEvent 实例进入终态这件事，供依赖触发等下游动作使用。
type FinishEvent struct {
	JobId      uint64
	InstanceId uint64
	Status     string // SUCCEED / FAILED / STOPPED
}

type Dispatcher struct {
	st  *store.Store
	reg *registry.Registry
	sig *cluster.Signal // 跨副本广播"队列变了"；nil = 单机，只在进程内唤醒

	wakeMu   sync.Mutex        // 保护 wake 的换 channel，见 Notify
	wake     chan struct{}     // 广播通道：fire() 或 report 产生新任务时 close 并重建
	rr       map[uint64]uint64 // jobID -> 轮询游标，ROUND_ROBIN 策略用
	rrm      sync.Mutex
	onFinish func(FinishEvent)
}

// New sig 传 nil 表示单副本部署（此时 Notify 只唤醒本机长轮询，行为与引入集群前一致）。
func New(st *store.Store, reg *registry.Registry, sig *cluster.Signal) *Dispatcher {
	return &Dispatcher{st: st, reg: reg, sig: sig, wake: make(chan struct{}), rr: make(map[uint64]uint64)}
}

// OnInstanceFinished 注册实例终态回调（依赖触发用）。回调在事务提交之后执行，
// 绝不能在 Report 的事务里跑：那会在同一个 sqlite 写事务里再开一个事务。
// main 里 Scheduler 依赖 Dispatcher 构造，只能用 setter 反向注入，避免包循环。
func (d *Dispatcher) OnInstanceFinished(fn func(FinishEvent)) {
	d.onFinish = fn
}

func (d *Dispatcher) fireFinishEvent(ev FinishEvent) {
	if d.onFinish == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("onInstanceFinished panic: %v", r)
		}
	}()
	d.onFinish(ev)
}

// cursor 取该 job 的下一个轮询下标（候选数可变，按当前长度取模）。
func (d *Dispatcher) cursor(jobID, n uint64) uint64 {
	d.rrm.Lock()
	defer d.rrm.Unlock()
	i := d.rr[jobID]
	d.rr[jobID]++
	return i % n
}

// Notify 唤醒所有阻塞在 Fetch 上的长轮询请求，并把"队列变了"广播给其他副本。
// 本机唤醒是即时的，DB 那一次 seq+1 只是告诉别的副本"别等到兜底轮询，现在就看"。
func (d *Dispatcher) Notify() {
	d.wakeLocal()
	if d.sig != nil {
		d.sig.Bump()
	}
}

// wakeLocal 只唤醒本进程内的长轮询。换 channel 必须整段在锁内：
// 别的副本信号、回报重试、停止实例都可能并发走到这里，
// 无锁的话两个 goroutine 会各自 close 同一个 channel 而 panic（close of closed channel）。
func (d *Dispatcher) wakeLocal() {
	d.wakeMu.Lock()
	defer d.wakeMu.Unlock()
	ch := d.wake
	d.wake = make(chan struct{})
	close(ch)
}

// watchCh 取当前用于等待的 channel（与 wakeLocal 同一把锁，保证不会读到"已被 close 但还没换掉"的那个）。
func (d *Dispatcher) watchCh() chan struct{} {
	d.wakeMu.Lock()
	defer d.wakeMu.Unlock()
	return d.wake
}

// WatchSignals 起后台循环监听别的副本的入队广播。
func (d *Dispatcher) WatchSignals(ctx context.Context) {
	if d.sig == nil {
		return
	}
	d.sig.Watch(ctx, d.wakeLocal)
}

// Fetch 长轮询拉取任务：先尝试立即认领，无任务则阻塞至新任务唤醒或超时。
// Fetch 长轮询拉取任务。返回的 cancels 是"该 worker 名下已被请求停止、尚未 ack"的任务 id：
// pull 模型没有反向连接，下行指令只能靠响应夹带，而指令本身存在 DB 里，server 重启不丢。
func (d *Dispatcher) Fetch(ctx context.Context, req *protocol.FetchTaskReq) ([]protocol.TaskDTO, []uint64, error) {
	maxNum := req.MaxTaskNum
	if maxNum <= 0 {
		maxNum = 5
	}
	deadline := time.Now().Add(time.Duration(req.WaitSeconds) * time.Second)
	for {
		// 先取本次要等的唤醒 channel，再去认领。顺序反了就有一个丢唤醒的窗口：
		// claim 返回空 → 这中间有人 Notify（close 掉当时的旧 channel 并换新）→ watchCh
		// 拿到新 channel → 等下去没人再 close 它。兜底只有 5s 复查（单副本配了 cluster
		// 密钥时还有 1s 一跳的跨副本信号），压测里就出现过整轮任务统一迟到 1.27s。
		// channel 一旦 close 就永远可读，所以提前取即可保证不漏、也不会多等。
		wake := d.watchCh()
		tasks, err := d.claim(req, maxNum)
		if err != nil {
			return nil, nil, err
		}
		cancels := d.PendingCancels(req.InstanceId)
		// 早退只看"刚下达"的停止指令：cancel 要等任务回报才从 DB 清掉，期间每一轮都非空，
		// 拿 len(cancels)>0 当早退条件会把长轮询退化成忙等（实测一个 30 秒任务被重复下发 53 次
		// 停止指令，每轮两条查询加一个 HTTP 往返，还会挤住 sqlite 的单写者把后来的
		// stop_request_time 写入拖慢）。错过这个窗口不丢指令：响应体始终带着全量 cancels，
		// 心跳（5s 一跳）也夹带同一份清单。
		fresh := len(cancels) > 0 && d.hasFreshCancels(req.InstanceId)
		if len(tasks) > 0 || fresh || time.Now().After(deadline) {
			return tasks, cancels, nil
		}
		wait := time.Until(deadline)
		if wait > 5*time.Second {
			wait = 5 * time.Second // 周期性复查，唤醒信号丢了也不至于饿死
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-wake:
		case <-time.After(wait):
		}
	}
}

// PendingCancels 返回该 worker 正在执行、且已收到停止指令但尚未回报的任务 ID。
func (d *Dispatcher) PendingCancels(workerInstanceId string) []uint64 {
	var ids []uint64
	if err := d.st.DB.Model(&store.InstanceTask{}).
		Where("worker_id = ? AND status = ? AND stop_request_time IS NOT NULL",
			workerInstanceId, protocol.TaskRunning).
		Pluck("id", &ids).Error; err != nil {
		log.Printf("PendingCancels: %v", err)
		return nil
	}
	return ids
}

// cancelWakeWindow 停止指令算"新到"、值得为它打断长轮询的时间窗。StopInstance 提交后会
// Notify 唤醒本机 poller，跨副本靠 cluster 信号（1s 一跳），所以正常路径上指令落库后
// 1~2s 内一定会赶上一次带 fresh 判定的轮询。
const cancelWakeWindow = 2 * time.Second

// hasFreshCancels 该 worker 名下是否有 cancelWakeWindow 之内刚被请求停止的任务。
// 只用于决定"要不要提前结束长轮询"，不参与"要不要下发"——下发看的是 PendingCancels。
func (d *Dispatcher) hasFreshCancels(workerInstanceId string) bool {
	var n int64
	if err := d.st.DB.Model(&store.InstanceTask{}).
		Where("worker_id = ? AND status = ? AND stop_request_time > ?",
			workerInstanceId, protocol.TaskRunning, store.Now().Add(-cancelWakeWindow)).
		Count(&n).Error; err != nil {
		log.Printf("hasFreshCancels: %v", err)
		return false
	}
	return n > 0
}

// StopInstance 停止实例：WAITING 任务直接终止，RUNNING 任务落停止意图等 worker 协作退出。
// 幂等：重复调用只会覆盖时间戳；已终态实例返回错误。
func (d *Dispatcher) StopInstance(instanceId uint64) error {
	_, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		var inst store.JobInstance
		if e := tx.First(&inst, instanceId).Error; e != nil {
			return struct{}{}, e
		}
		switch inst.Status {
		case protocol.InstanceSucceed, protocol.InstanceFailed, protocol.InstanceStopped:
			return struct{}{}, fmt.Errorf("instance %d already finished (%s)", instanceId, inst.Status)
		}
		now := store.Now()
		// 未被认领的任务当场终止，不必等 worker；RUNNING 的只能等它协作退出
		if e := tx.Model(&store.InstanceTask{}).
			Where("instance_id = ? AND status = ?", instanceId, protocol.TaskWaiting).
			Updates(map[string]any{"status": protocol.TaskStopped, "result": "stopped before dispatch",
				"modify_time": now}).Error; e != nil {
			return struct{}{}, e
		}
		c, err := countTasks(tx, instanceId)
		if err != nil {
			return struct{}{}, err
		}
		if err := syncInstance(tx, instanceId, c, now); err != nil {
			return struct{}{}, err
		}
		if c.Running > 0 {
			if e := tx.Model(&store.InstanceTask{}).
				Where("instance_id = ? AND status = ?", instanceId, protocol.TaskRunning).
				Updates(map[string]any{"stop_request_time": now, "modify_time": now}).Error; e != nil {
				return struct{}{}, e
			}
			// 先落到 STOPPING：settleInstance 靠"实例正在停止中"区分"用户停的"与
			// "父任务重派作废掉的"，不能等到最后再改这个状态。
			return struct{}{}, tx.Model(&store.JobInstance{}).Where("id = ?", instanceId).
				Updates(map[string]any{"status": protocol.InstanceStopping, "modify_time": now}).Error
		}
		inst.Status = protocol.InstanceStopping
		if _, e := settleInstance(tx, &inst, c, now); e != nil {
			return struct{}{}, e
		}
		return struct{}{}, nil
	})
	if err == nil {
		d.Notify() // 唤醒长轮询，让 cancel 指令立刻随响应下发
	}
	return err
}

// claim 认领 app 下的 WAITING 任务，并把它们交给**实际抢到**的那个 worker。
//
// 为什么逐行条件 UPDATE 而不是"批量 SELECT 后整批改成 RUNNING"：SELECT 与 UPDATE 之间没有
// 行锁（sqlite 之外的 mysql/pg 在 READ COMMITTED 下两个 worker 的 SELECT 会看到同一批 WAITING 行），
// 先提交的一方改了状态，后提交的一方仍把手里那批任务发给了自己的 worker ⇒ 同一任务被执行两次。
// 所以必须以 `RowsAffected==1` 作为"认领成功"的唯一依据，抢不到的行不下发。
// 不用 FOR UPDATE SKIP LOCKED：sqlite 不支持，要三库一致。
func (d *Dispatcher) claim(req *protocol.FetchTaskReq, maxNum int) ([]protocol.TaskDTO, error) {
	if ok, err := d.reg.HasWorkers(req.AppId); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNoWorker
	}
	return d.st.Tx(func(tx *gorm.DB) ([]protocol.TaskDTO, error) {
		tasks, err := candidates(tx, req, maxNum)
		if err != nil {
			return nil, err
		}
		now := store.Now()
		ids := make([]uint64, 0, len(tasks))
		byID := make(map[uint64]*store.InstanceTask, len(tasks))
		for i := range tasks {
			ids = append(ids, tasks[i].ID)
			byID[tasks[i].ID] = &tasks[i]
		}
		won, err := takeRows(tx, ids, req.InstanceId, now)
		if err != nil {
			return nil, err
		}
		out := make([]protocol.TaskDTO, 0, len(won))
		for _, id := range won {
			t := byID[id]
			out = append(out, protocol.TaskDTO{
				TaskId: t.ID, JobId: t.JobId, InstanceId: t.InstanceId,
				AppId: t.AppId, Processor: t.Processor, JobParams: t.JobParams,
				ProcessorType: t.ProcessorType, ExecuteType: t.ExecuteType,
				TaskKey: t.TaskKey, TaskName: t.TaskName, SubTaskContent: t.SubTaskContent,
				FixedParams: t.FixedParams, LogLevel: t.LogLevel,
				Attempts: t.Attempts, RetryTimes: t.MaxAttempts - t.Attempts,
			})
		}
		return out, nil
	})
}

// candidates 选出本轮候选任务：先取指名给本机的，剩余名额再取自由任务。
//
// 为什么拆成两条查询：原来一个 `target_worker_id = ? OR target_worker_id = ”` 的 OR
// 让优化器没法把 (app_id,status,dispatch_time) 索引吃满（要回表判 OR、还得 filesort 排序），
// 队列一积压就是每次认领全量扫描；拆开后两条都是纯前缀匹配 + 天然有序。
// 顺序上让"指名本机"的先走：这类任务本来要饿到 ReleaseStaleTargets（60s）才可能被别人捡走。
func candidates(tx *gorm.DB, req *protocol.FetchTaskReq, maxNum int) ([]store.InstanceTask, error) {
	take := func(target string, limit int) ([]store.InstanceTask, error) {
		var rows []store.InstanceTask
		err := tx.Where("app_id = ? AND status = ? AND dispatch_time <= ? AND target_worker_id = ?",
			req.AppId, protocol.TaskWaiting, store.Now(), target).
			Order("dispatch_time asc").Limit(limit).Find(&rows).Error
		return rows, err
	}
	taken, err := take(req.InstanceId, maxNum)
	if err != nil {
		return nil, err
	}
	if rest := maxNum - len(taken); rest > 0 {
		free, err := take("", rest)
		if err != nil {
			return nil, err
		}
		taken = append(taken, free...)
	}
	return taken, nil
}

// takeRows 把 ids 里的行逐个做"仅当仍为 WAITING 才认领"的条件更新，
// 返回真正归本 worker 的 id（顺序保持入参顺序）。抢不到的行说明已被别人领走。
func takeRows(tx *gorm.DB, ids []uint64, workerId string, now time.Time) ([]uint64, error) {
	won := make([]uint64, 0, len(ids))
	for _, id := range ids {
		res := tx.Model(&store.InstanceTask{}).
			Where("id = ? AND status = ?", id, protocol.TaskWaiting).
			Updates(map[string]any{
				"status": protocol.TaskRunning, "worker_id": workerId, "modify_time": now,
			})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			won = append(won, id)
		}
	}
	return won, nil
}

// Report 处理 Worker 回报：更新任务/实例状态，失败未超重试上限则重新入队。
func (d *Dispatcher) Report(ctx context.Context, req *protocol.ReportTaskReq) error {
	retryNeeded := false
	var finished FinishEvent
	_, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		var t store.InstanceTask
		if e := tx.First(&t, req.TaskId).Error; e != nil {
			return struct{}{}, e
		}
		// 终态不可逆：重复回报（SDK 带重试）与迟到回报（失联 worker 复活补报）一律吞掉，
		// 否则实例计数被二次扣减、已作废的子任务会被翻案。
		if protocol.IsTerminalTask(t.Status) {
			return struct{}{}, nil
		}
		// 结果会被写进行并回给控制台，不裁就是让处理器里一个 println 撑爆数据库
		req.Result = truncResult(req.Result)
		// 只认任务当前归属 worker 的回报：
		// 1) 同项目内别的 worker（都持同一份 app 密码）无法越权把别人正在跑的任务判成成功/失败；
		// 2) 失联 worker 复活后的迟到回报不会污染已被 ReassignLostTasks 重派出去的任务。
		// 这里静默吞掉而不是报错：让误报方拿到 code=0，避免它无限重试。
		if t.WorkerId != req.InstanceId {
			log.Printf("report of task %d ignored: claimed by %q, reported by %q (status %s)",
				t.ID, t.WorkerId, req.InstanceId, t.Status)
			return struct{}{}, nil
		}
		now := store.Now()
		// 兼容只带 success 的旧 SDK；已请求停止的任务即使回报失败也按 STOPPED 收尾
		outcome := strings.ToUpper(strings.TrimSpace(req.Status))
		if outcome == "" {
			outcome = protocol.TaskSucceed
			if !req.Success {
				outcome = protocol.TaskFailed
			}
		}
		if outcome == protocol.TaskFailed && t.StopRequestTime != nil {
			outcome = protocol.TaskStopped
		}
		// 实例被本次回报推到终态时记下来，事务提交后再回调依赖触发
		terminal := func(status string, err error) (struct{}, error) {
			if err != nil {
				return struct{}{}, err
			}
			if status != "" {
				finished = FinishEvent{JobId: t.JobId, InstanceId: t.InstanceId, Status: status}
			}
			return struct{}{}, nil
		}
		switch outcome {
		case protocol.TaskStopped:
			if e := tx.Model(&t).Updates(map[string]any{
				"status": protocol.TaskStopped, "result": req.Result,
				"stop_request_time": nil, "modify_time": now,
			}).Error; e != nil {
				return struct{}{}, e
			}
			return terminal(finishInstance(tx, &t, protocol.TaskStopped, now))
		case protocol.TaskSucceed:
			if e := tx.Model(&t).Updates(map[string]any{
				"status": protocol.TaskSucceed, "result": req.Result, "modify_time": now,
			}).Error; e != nil {
				return struct{}{}, e
			}
			return terminal(finishInstance(tx, &t, protocol.TaskSucceed, now))
		}
		// 失败：还能重试就重新排队
		if t.Attempts < t.MaxAttempts {
			// 广播子任务重试用"原地重试"：它的 target_worker_id 就是这一份广播的语义本身
			// （每台一份），换机重试等于让同一份工作在别的节点再跑一遍。
			updates := map[string]any{
				"status": protocol.TaskWaiting, "attempts": t.Attempts + 1,
				"worker_id": "", "stop_request_time": nil,
				"result": req.Result, "dispatch_time": now, "modify_time": now,
			}
			if t.TaskName != protocol.BroadcastTaskName {
				updates["target_worker_id"] = ""
			}
			if e := tx.Model(&t).Updates(updates).Error; e != nil {
				return struct{}{}, e
			}
			// 重跑会重新 map 一遍，旧的未收敛后代必须作废，否则双跑
			if n, e := discardDescendants(tx, t.ID, now); e != nil {
				return struct{}{}, e
			} else if n > 0 {
				c, e := countTasks(tx, t.InstanceId)
				if e != nil {
					return struct{}{}, e
				}
				if e := syncInstance(tx, t.InstanceId, c, now); e != nil {
					return struct{}{}, e
				}
			}
			retryNeeded = true
			return struct{}{}, nil
		}
		if e := tx.Model(&t).Updates(map[string]any{
			"status": protocol.TaskFailed, "result": req.Result, "modify_time": now,
		}).Error; e != nil {
			return struct{}{}, e
		}
		status, e := finishInstance(tx, &t, protocol.TaskFailed, now)
		return terminal(status, e)
	})
	if err == nil {
		if retryNeeded {
			d.Notify()
		}
		if finished.InstanceId != 0 {
			d.fireFinishEvent(finished)
		}
	}
	return err
}

// ReassignLostTasks 将失联 worker 上 RUNNING 超时任务收回队列（M2 完善判定，先留接口）。
// 已被请求停止的任务不复活重跑，直接就地置 STOPPED —— 否则宕机重启会把用户刚按下的停止又跑一遍。
func (d *Dispatcher) ReassignLostTasks(olderThan time.Duration) error {
	now := store.Now()
	cutoff := now.Add(-olderThan)

	// 先取快照再改状态：避免同批任务在后续巡检里被重复计入实例终态
	var stopLost []store.InstanceTask
	if err := d.st.DB.Where("status = ? AND dispatch_time < ? AND stop_request_time IS NOT NULL",
		protocol.TaskRunning, cutoff).Find(&stopLost).Error; err != nil {
		return err
	}
	// 失联且未请求停止的任务分两类处理。广播子任务是"每台一份"这件事本身，换机重跑等于
	// 让同一份工作在一个从没执行过它的节点上再执行一遍（兼容协议明确禁止这样做），
	// 所以它不重派，直接判失败；其余任务照旧退回队列换机重试。
	var lost []store.InstanceTask
	if err := d.st.DB.Where("status = ? AND dispatch_time < ? AND stop_request_time IS NULL",
		protocol.TaskRunning, cutoff).Find(&lost).Error; err != nil {
		return err
	}
	requeued, abandoned := 0, 0
	for i := range lost {
		t := lost[i]
		if t.TaskName == protocol.BroadcastTaskName {
			res := d.st.DB.Model(&store.InstanceTask{}).
				Where("id = ? AND status = ?", t.ID, protocol.TaskRunning).
				Updates(map[string]any{"status": protocol.TaskFailed,
					"result": truncResult("执行节点失联，广播任务不重派"), "modify_time": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				continue // 已被别的巡检或回报处理过
			}
			abandoned++
			if _, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
				if n, e := discardDescendants(tx, t.ID, now); e != nil {
					return struct{}{}, e
				} else if n > 0 {
					c, e := countTasks(tx, t.InstanceId)
					if e != nil {
						return struct{}{}, e
					}
					if e := syncInstance(tx, t.InstanceId, c, now); e != nil {
						return struct{}{}, e
					}
				}
				// 广播子任务不重试：MaxAttempts 在这里被绕过，重派才是它绕不开的那条路
				cp := t
				cp.Status = protocol.TaskFailed
				if _, e := finishInstance(tx, &cp, protocol.TaskFailed, now); e != nil {
					return struct{}{}, e
				}
				return struct{}{}, nil
			}); err != nil {
				log.Printf("abandon lost broadcast task %d: %v", t.ID, err)
			}
			continue
		}
		res := d.st.DB.Model(&store.InstanceTask{}).
			Where("id = ? AND status = ?", t.ID, protocol.TaskRunning).
			Updates(map[string]any{"status": protocol.TaskWaiting, "worker_id": "",
				"target_worker_id": "", "modify_time": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			continue
		}
		requeued++
		// 重跑会重新 map 一遍，旧的未收敛后代必须作废
		if _, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
			n, e := discardDescendants(tx, t.ID, now)
			if e != nil || n == 0 {
				return struct{}{}, e
			}
			c, e := countTasks(tx, t.InstanceId)
			if e != nil {
				return struct{}{}, e
			}
			return struct{}{}, syncInstance(tx, t.InstanceId, c, now)
		}); err != nil {
			log.Printf("discard descendants of requeued task %d: %v", t.ID, err)
		}
	}

	for i := range stopLost {
		t := stopLost[i]
		if _, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
			if e := tx.Model(&store.InstanceTask{}).Where("id = ? AND status = ?", t.ID, protocol.TaskRunning).
				Updates(map[string]any{"status": protocol.TaskStopped, "result": "worker lost before ack",
					"stop_request_time": nil, "modify_time": now}).Error; e != nil {
				return struct{}{}, e
			}
			if n, e := discardDescendants(tx, t.ID, now); e != nil {
				return struct{}{}, e
			} else if n > 0 {
				c, e := countTasks(tx, t.InstanceId)
				if e != nil {
					return struct{}{}, e
				}
				if e := syncInstance(tx, t.InstanceId, c, now); e != nil {
					return struct{}{}, e
				}
			}
			// 只可能被停的任务命中，终态恒为 STOPPED，不触发下游
			if _, e := finishInstance(tx, &t, protocol.TaskStopped, now); e != nil {
				return struct{}{}, e
			}
			return struct{}{}, nil
		}); err != nil {
			log.Printf("reassign lost task %d: %v", t.ID, err)
		}
	}
	if requeued > 0 || abandoned > 0 || len(stopLost) > 0 {
		d.Notify()
	}
	return nil
}

// ReleaseStaleTargets 把预选机器迟迟未取走的 WAITING 任务退回自由池，
// 避免分发策略预选的节点宕机/长时间满载时任务饿死。
// 广播子任务除外：它的预选就是它的语义，放出去会让别的节点执行第二份。
func (d *Dispatcher) ReleaseStaleTargets(olderThan time.Duration) error {
	cutoff := store.Now().Add(-olderThan)
	res := d.st.DB.Model(&store.InstanceTask{}).
		Where("status = ? AND target_worker_id <> '' AND dispatch_time < ? AND task_name <> ?",
			protocol.TaskWaiting, cutoff, protocol.BroadcastTaskName).
		Updates(map[string]any{"target_worker_id": "", "modify_time": store.Now()})
	if res.RowsAffected > 0 {
		d.Notify()
	}
	return res.Error
}
