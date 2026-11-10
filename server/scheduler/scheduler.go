// Package scheduler 周期扫描到期 Job，生成实例与待拉取任务（M1 单节点版；
// M3 引入选主/分片后多 Server 安全并行）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/cluster"
	"gojob/server/store"
)

// ErrJobRunning 手动触发时任务仍在执行且开启了 SkipWhenRunning。
var ErrJobRunning = errors.New("job is running and skip_when_running is enabled")

// ErrJobPaused 暂停的任务不接受手动触发：认领层按 job_infos.enable 硬过滤，
// 即使建出任务也永远拉不到，直接拒绝比留一个卡在 WAITING 的实例清楚。
var ErrJobPaused = errors.New("任务已暂停，不能手动触发；请先恢复任务")

// noWorkerResult 触发时项目没有在线节点的实例结果，对齐兼容协议的 NO_WORKER_AVAILABLE。
const noWorkerResult = "无可用 worker：该项目当前没有在线节点"

// SecondOptional：同时支持标准 5 段 cron 与兼容协议的 6 段秒级表达式风格
var cronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour |
	cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// apiNever：API（手动触发）类型的下次自动触发时间哨兵，永远不到期。
var apiNever = time.Date(9999, 1, 1, 0, 0, 0, 0, time.Local)

func NextTime(expr string, from time.Time) (time.Time, error) {
	sch, err := cronParser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron %q: %w", expr, err)
	}
	return sch.Next(from), nil
}

// MaxFrequencyIntervalMs 取兼容协议配置项 FREQUENT_JOB_MAX_INTERVAL 的默认值：
// 固定频率/固定延迟的最大周期（毫秒）。周期必须落在扫描窗口量级内才有意义，
// 超过该上限的任务可能长时间不被粗扫装载，兼容目标直接在保存阶段拒绝。
const MaxFrequencyIntervalMs = 120_000

// parseIntervalMs 解析 FIXED_RATE / FIXED_DELAY 的周期（毫秒），与兼容协议同语义。
func parseIntervalMs(typ, expr string) (int64, error) {
	ms, err := strconv.ParseInt(strings.TrimSpace(expr), 10, 64)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("%s 表达式需为正整数毫秒，got %q", typ, expr)
	}
	if ms > MaxFrequencyIntervalMs {
		return 0, fmt.Errorf("%s 周期不能超过 %d ms，got %d", typ, MaxFrequencyIntervalMs, ms)
	}
	return ms, nil
}

// NextByType 按时间表达式类型计算下一次触发时间，单位与兼容协议一致（毫秒）。
//   - CRON：解析 cron 求 next
//   - FIXED_RATE：next = 上次计划时间 base + 周期(ms)，等间隔不累积漂移；
//     若已落后当前时间则从 now 重算，避免宕机后疯狂追补
//   - FIXED_DELAY：next = 本次实际触发时刻 now + 周期(ms)，即"跑完再等一个周期"
//   - API / DEPENDENCY / WORKFLOW / 空：不由时间驱动，返回 apiNever
func NextByType(typ, expr string, base, now time.Time) (time.Time, error) {
	switch strings.ToUpper(strings.TrimSpace(typ)) {
	case "", "API", "WORKFLOW", protocol.SourceDependency:
		return apiNever, nil // 不由时间驱动：手动/OpenAPI 触发，或被上游实例的成功回调拉起
	case "FIXED_RATE":
		ms, err := parseIntervalMs("fixed_rate", expr)
		if err != nil {
			return time.Time{}, err
		}
		next := base.Add(time.Duration(ms) * time.Millisecond)
		if !next.After(now) {
			next = now.Add(time.Duration(ms) * time.Millisecond)
		}
		return next, nil
	case "FIXED_DELAY":
		ms, err := parseIntervalMs("fixed_delay", expr)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(time.Duration(ms) * time.Millisecond), nil
	case "DAILY_TIME_INTERVAL":
		return nextDailyTimeInterval(expr, base, now)
	case "CRON":
		return NextTime(expr, now)
	default:
		return time.Time{}, fmt.Errorf("unknown time_expression_type %q", typ)
	}
}

// Preview 返回从 now 起最近 count 次计划触发时间，供控制台预览（毫秒精度）。
// API 类型无自动触发时间，返回空。
func Preview(typ, expr string, now time.Time, count int) ([]time.Time, error) {
	if count <= 0 || count > 50 {
		count = 15
	}
	switch strings.ToUpper(strings.TrimSpace(typ)) {
	case "", "API", "WORKFLOW", protocol.SourceDependency:
		return nil, nil
	case "FIXED_RATE", "FIXED_DELAY":
		ms, err := parseIntervalMs(strings.ToLower(typ), expr)
		if err != nil {
			return nil, err
		}
		out := make([]time.Time, count)
		for i := range out {
			out[i] = now.Add(time.Duration(ms) * time.Millisecond * time.Duration(i+1))
		}
		return out, nil
	case "DAILY_TIME_INTERVAL":
		out := make([]time.Time, count)
		t := now
		for i := range out {
			var err error
			t, err = nextDailyTimeInterval(expr, t, now)
			if err != nil {
				return nil, err
			}
			out[i] = t
		}
		return out, nil
	case "CRON":
		sch, err := cronParser.Parse(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid cron %q: %w", expr, err)
		}
		out := make([]time.Time, count)
		t := now
		for i := range out {
			t = sch.Next(t)
			out[i] = t
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown time_expression_type %q", typ)
	}
}

// 粗扫周期：每次装载 [now, now+scanWindow) 内到期的任务进时间轮。
// 与兼容协议的 15s 扫描同理——扫描只决定"何时被装载"，
// 实际触发时刻由时间轮按 next_trigger_time 精确到 tickSize。
const scanInterval = 5 * time.Second

// wheelTickSize 调度精度上限：10ms。
const wheelTickSize = 10 * time.Millisecond

const wheelSlots = 4096

type Scheduler struct {
	st     *store.Store
	leases *cluster.Leases             // 多副本归属账本；nil = 单机模式（自己就是唯一副本）
	notify func()                      // 新任务入队后唤醒长轮询
	pick   func(*store.JobInfo) string // 分发策略预选执行节点，空串=任意节点可认领
	// fanout 广播的落点集合：返回该项目下每个在线节点的 worker instance_id。
	// 与 pick 的区别是它不做策略选择——"每台一份"本身就是分发结果，再选一次反而漏掉节点。
	fanout func(*store.JobInfo) []string
	// probeWorkers 项目当前是否有在线节点；nil = 不探测（测试/单机简化）。
	// 触发时无在线节点就当场把实例判失败，对齐兼容协议的 no worker available，
	// 不留在队列里等节点上线后补跑。
	probeWorkers func(appId string) (bool, error)

	wheel *Wheel
	mu    sync.Mutex
	// armed: jobId -> 已挂轮的下次触发时间（毫秒精度），用于去重与失效判断
	armed map[uint64]time.Time
	// appOf: jobId -> appId，只为在失去归属时快速撤掉该项目的挂轮，不承担正确性判断。
	appOf map[uint64]string
}

// New leases 传 nil 表示单副本部署（此时所有项目都归本机调度，行为与引入集群前完全一致）。
func New(st *store.Store, leases *cluster.Leases, notify func(), pick func(*store.JobInfo) string,
	fanout func(*store.JobInfo) []string, probeWorkers func(appId string) (bool, error)) *Scheduler {
	return &Scheduler{
		st:           st,
		leases:       leases,
		notify:       notify,
		pick:         pick,
		fanout:       fanout,
		probeWorkers: probeWorkers,
		wheel:        NewWheel(wheelTickSize, wheelSlots),
		armed:        make(map[uint64]time.Time),
		appOf:        make(map[uint64]string),
	}
}

// owns 是否由本机调度该项目。
func (s *Scheduler) owns(appId string) bool {
	return s.leases == nil || s.leases.Owns(appId)
}

// DropApp 失去某项目归属时撤掉它的全部挂轮（租约管理器的回调）。
// 不撤的话，僵尸主仍会在时间轮到点时走到 fire，只是必然被 fencing/CAS 拒掉——白拿一次写冲突。
func (s *Scheduler) DropApp(appId string) {
	s.mu.Lock()
	var ids []uint64
	for id, a := range s.appOf {
		if a == appId {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		delete(s.armed, id)
		delete(s.appOf, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.wheel.Cancel(id)
	}
}

// Run 两层调度：粗扫线程周期性装载到期任务，时间轮精确触发。
func (s *Scheduler) Run(ctx context.Context) {
	s.wheel.Start(ctx)
	defer s.wheel.Stop()

	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	s.scan() // 启动即装载一次，避免首扫等待一个周期
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scan()
		}
	}
}

// scan 粗筛：把 next_trigger_time 落在 [now, now+scanInterval] 的启用 Job 挂上时间轮；
// 同时清理计划已变更（被编辑/暂停）的失效挂轮。
func (s *Scheduler) scan() {
	now := store.Now()
	q := s.st.DB.Where("enable = ? AND next_trigger_time <= ?", true, now.Add(scanInterval))
	// 只装载本机归属的项目——这是集群下"一次触发只有一个副本生成实例"的结构性保证，
	// 与兼容协议的 "select id from AppInfoDO where currentServer = :currentServer" 同构。
	if s.leases != nil {
		owned := s.leases.Owned()
		if len(owned) == 0 {
			owned = []string{""} // 给 IN 一个非空但取不到东西的值，下面的撤轮逻辑照常跑
		}
		q = q.Where("app_id IN ?", owned)
	}
	var jobs []store.JobInfo
	if err := q.Order("next_trigger_time asc").Limit(2000).Find(&jobs).Error; err != nil {
		log.Printf("scheduler scan: %v", err)
		return
	}
	seen := make(map[uint64]bool, len(jobs))
	for i := range jobs {
		job := jobs[i]
		seen[job.ID] = true
		s.arm(&job)
	}
	// 已挂轮但本轮未被扫到（暂停/改期到窗口外/删除）→ 撤轮
	s.mu.Lock()
	for id := range s.armed {
		if !seen[id] {
			delete(s.armed, id)
			delete(s.appOf, id)
			s.wheel.Cancel(id)
		}
	}
	s.mu.Unlock()
}

// arm 按 job.NextTriggerTime 挂轮；计划时间未变则跳过（防重复装载）。
func (s *Scheduler) arm(job *store.JobInfo) {
	target := truncMs(job.NextTriggerTime)
	s.mu.Lock()
	if old, ok := s.armed[job.ID]; ok && old.Equal(target) {
		s.mu.Unlock()
		return
	}
	s.armed[job.ID] = target
	s.appOf[job.ID] = job.AppId
	s.mu.Unlock()

	id := job.ID
	s.wheel.Add(id, target, func() { s.onFire(id, target) })
}

// Reschedule 供管理面在 Job 新建/编辑/启停后立即重挂时间轮，
// 不等下一个扫描周期（把配置变更的生效延迟从 scanInterval 压到毫秒级）。
func (s *Scheduler) Reschedule(jobID uint64) {
	var job store.JobInfo
	if err := s.st.DB.First(&job, jobID).Error; err != nil {
		s.mu.Lock()
		delete(s.armed, jobID)
		delete(s.appOf, jobID)
		s.mu.Unlock()
		s.wheel.Cancel(jobID)
		return
	}
	if !job.Enable {
		s.mu.Lock()
		delete(s.armed, jobID)
		delete(s.appOf, jobID)
		s.mu.Unlock()
		s.wheel.Cancel(jobID)
		return
	}
	s.arm(&job)
}

// onFire 时间轮到点：校验计划未被外部改动，触发一次并推进/重挂。
func (s *Scheduler) onFire(jobID uint64, expected time.Time) {
	var job store.JobInfo
	if err := s.st.DB.First(&job, jobID).Error; err != nil {
		s.mu.Lock()
		delete(s.armed, jobID)
		delete(s.appOf, jobID)
		s.mu.Unlock()
		return // 已删除：静默丢弃
	}
	if !job.Enable {
		s.mu.Lock()
		delete(s.armed, jobID)
		delete(s.appOf, jobID)
		s.mu.Unlock()
		return
	}
	// 归属已易主（租约被别的副本接管）→ 直接撤轮，不必去撞事务里的 fencing。
	if !s.owns(job.AppId) {
		s.mu.Lock()
		delete(s.armed, jobID)
		delete(s.appOf, jobID)
		s.mu.Unlock()
		s.wheel.Cancel(jobID)
		return
	}
	// 计划已被编辑/推进过（本回调过期）→ 以最新计划重挂，不重复触发
	if !truncMs(job.NextTriggerTime).Equal(expected) {
		s.arm(&job)
		return
	}

	now := store.Now()
	next, err := NextByType(job.TimeExpressionType, job.TimeExpression, expected, now)
	if err != nil {
		log.Printf("job %d: %v", job.ID, err)
		return
	}
	src := job.TimeExpressionType
	if src == "" {
		src = "CRON"
	}
	switch skipped, _, err := s.fire(&job, next, src, "", 0); {
	case errors.Is(err, ErrTriggerTaken):
		// 多副本的正常竞态：这个触发时刻归别的副本消费，静默跳过即可。
	case err != nil:
		log.Printf("fire job %d: %v", job.ID, err)
	case skipped:
		log.Printf("job %d skipped: previous instance still running", job.ID)
	}

	// 立即为下一次触发挂轮：短周期（< 扫描窗口）任务不依赖下轮粗扫
	job.NextTriggerTime = next
	s.arm(&job)
}

// truncMs 把时间截断到毫秒，与 DB 存储精度及时间轮 tick 对齐，避免比较抖动。
func truncMs(t time.Time) time.Time {
	return t.Truncate(time.Millisecond)
}

// ErrTriggerTaken 本次触发已被别的副本消费，或本机已失去项目归属。不是故障：
// 调用方按"跳过"处理即可，实例由赢的那个副本创建。
var ErrTriggerTaken = errors.New("scheduler: trigger taken by another server")

// fire 创建实例 + 一条待拉取任务，并原子推进 next_trigger_time。
//
// 多副本下的三道防线（越靠后越兜底）：
//  1. 装载归属：scan 只把本机持有租约的项目挂上时间轮，正常路径下同一触发只有一个副本走到这里；
//  2. CAS 推进：next_trigger_time 只在"仍等于我这次读到的值"时才前移，抢输的副本
//     RowsAffected==0 → 整个事务回滚，绝不产生第二个实例。这道防线与归属无关，
//     哪怕两个副本都以为自己是对的，也只有一个能把这个时刻消费掉；
//  3. 租约 fencing：事务内条件确认 (holder, epoch) 仍是本机，挡住"租约早过期但进程还活着"
//     的僵尸主。兼容协议里没有这个防护（网络分区后旧主仍在调度）。
//
// 手动触发（API）与依赖触发不在任何副本独占的时间轴上——一次请求就是一次事实，
// 所以既不推进计划时间也不参与 fencing，任何副本都能执行。
func (s *Scheduler) fire(job *store.JobInfo, next time.Time, source, instanceParams string, upstreamInstanceId uint64) (skipped bool, instanceId uint64, err error) {
	advances := !next.Equal(job.NextTriggerTime)
	expected := job.NextTriggerTime
	// 分发策略预选放在事务外：它只是只读在线节点，没必要为此延长事务持锁。
	var target string
	if s.pick != nil {
		target = s.pick(job)
	}
	// 广播的落点集合同样是入队那一刻的快照：之后新上线的节点不会补到这一份广播
	// （兼容协议的 WorkerDetector 会在运行期补，但那是"实例已经开跑后再凭空多一份"，
	// 对幂等敏感的处理器更危险，这里选择更保守的形态并如实记进文档）。
	var broadcast []string
	if job.ExecuteType == protocol.ExecuteBroadcast && s.fanout != nil {
		broadcast = s.fanout(job)
	}
	// 广播除外：零节点广播按既有语义记 SUCCEED（根任务就是终态、不产生待领取任务），
	// 本来就不会积压，不该被改判失败。
	noWorker := false
	if s.probeWorkers != nil && job.ExecuteType != protocol.ExecuteBroadcast {
		ok, err := s.probeWorkers(job.AppId)
		switch {
		case err != nil:
			// 探测失败按"有节点"处理：宁可排队，也不因一次查询抖动把整批实例判死。
			log.Printf("probe workers for app %s: %v", job.AppId, err)
		case !ok:
			noWorker = true
		}
	}
	_, err = s.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		if advances && s.leases != nil && !s.leases.Fence(tx, job.AppId) {
			return struct{}{}, ErrTriggerTaken
		}
		var running int64
		if e := tx.Model(&store.JobInstance{}).
			Where("job_id = ? AND status IN ?", job.ID,
				[]string{protocol.InstanceWaiting, protocol.InstanceRunning}).
			Count(&running).Error; e != nil {
			return struct{}{}, e
		}
		advance := func() error {
			if !advances {
				return nil // 手动/依赖触发不动计划时间
			}
			res := tx.Model(&store.JobInfo{}).
				Where("id = ? AND next_trigger_time = ?", job.ID, expected).
				Update("next_trigger_time", next)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return ErrTriggerTaken // 这个触发时刻已被消费，本次什么都不留
			}
			return nil
		}
		if job.SkipWhenRunning && running > 0 {
			if source == "API" {
				return struct{}{}, ErrJobRunning
			}
			skipped = true
			return struct{}{}, advance() // 执行中：跳过本次，只推进时间
		}
		if job.InstanceNumLimit > 0 && int(running) >= job.InstanceNumLimit {
			skipped = true
			return struct{}{}, advance() // 超限跳过本次触发，只推进时间
		}
		now := store.Now()
		params := instanceParams
		if params == "" {
			params = job.InstanceParams // 任务配置的默认运行时参数
		}
		logLevel := job.LogLevel
		if logLevel == "" {
			logLevel = "INFO"
		}
		execType := job.ExecuteType
		if execType == "" {
			execType = protocol.ExecuteStandalone
		}
		instStatus, instResult, runningTimes := protocol.InstanceWaiting, "", 1
		if noWorker {
			instStatus, instResult, runningTimes = protocol.InstanceFailed, noWorkerResult, 0
		}
		inst := store.JobInstance{
			JobId: job.ID, AppId: job.AppId, JobName: job.JobName,
			Processor: job.Processor, JobParams: job.JobParams,
			Status: instStatus, Result: instResult, Source: source, FixedParams: params,
			ExecuteType:  execType,
			TotalTaskNum: 1, RunningTimes: runningTimes,
			CreateTime: now, ModifyTime: now,
		}
		if upstreamInstanceId > 0 {
			inst.UpstreamInstanceId = &upstreamInstanceId
		}
		if e := tx.Create(&inst).Error; e != nil {
			return struct{}{}, e
		}
		// 事务里先后不影响原子性：抢输时上面那条实例插入会一起回滚。
		if e := advance(); e != nil {
			return struct{}{}, e
		}
		instanceId = inst.ID
		taskStatus, taskResult := protocol.TaskWaiting, ""
		if noWorker {
			taskStatus, taskResult = protocol.TaskFailed, noWorkerResult
		}
		task := store.InstanceTask{
			JobId: job.ID, InstanceId: inst.ID, AppId: job.AppId,
			Processor: job.Processor, JobParams: job.JobParams, FixedParams: params,
			ProcessorType: job.ProcessorType, ExecuteType: execType,
			TaskKey:      protocol.RootTaskKey,
			TaskName:     protocol.RootTaskName,
			Status:       taskStatus,
			Result:       taskResult,
			Attempts:     1,
			MaxAttempts:  job.MaxRetryTimes + 1,
			LogLevel:     logLevel,
			DispatchTime: now, CreateTime: now, ModifyTime: now,
		}
		// 广播的根任务不派给任何 worker：兼容协议里它只跑 preProcess，真正的活是"每台一份"
		// 的子任务。gojob 的处理器没有 preProcess 这一层，所以根任务建出来就是终态，
		// 它存在的意义是让"一个实例一棵树"这个形状在所有执行类型下统一。
		if execType == protocol.ExecuteBroadcast {
			task.Status = protocol.TaskSucceed
			task.Result = fmt.Sprintf("广播未执行：该项目当前没有在线节点")
			if len(broadcast) > 0 {
				task.Result = fmt.Sprintf("广播 %d 个执行节点", len(broadcast))
			}
			task.ModifyTime = now
		} else if !noWorker {
			task.TargetWorkerId = target
		}
		if e := tx.Create(&task).Error; e != nil {
			return struct{}{}, e
		}
		for i, workerId := range broadcast {
			sub := task
			sub.ID = 0
			sub.Status = protocol.TaskWaiting
			sub.Result = ""
			sub.Attempts = 1
			sub.TargetWorkerId = workerId // 指名到机器：candidates() 的"指名本机"通道原样复用
			sub.TaskKey = fmt.Sprintf("%s.%d", protocol.RootTaskKey, i+1)
			sub.TaskName = protocol.BroadcastTaskName
			sub.ParentTaskId = task.ID
			sub.CreateTime, sub.ModifyTime, sub.DispatchTime = now, now, now
			if e := tx.Create(&sub).Error; e != nil {
				return struct{}{}, e
			}
		}
		if execType == protocol.ExecuteBroadcast {
			n := len(broadcast)
			if n == 0 {
				// 一个节点都没有：这棵树一行未终态任务都不剩，必须当场把实例收成 SUCCEED，
				// 否则它会以"还有 1 个未结束任务"挂着，而永远不会有 worker 来回报它。
				inst.Status = protocol.InstanceSucceed
				if e := tx.Model(&store.JobInstance{}).Where("id = ?", inst.ID).
					Updates(map[string]any{"status": protocol.InstanceSucceed,
						"running_times": 0, "result": task.Result, "modify_time": now}).Error; e != nil {
					return struct{}{}, e
				}
			} else {
				inst.TotalTaskNum, inst.RunningTimes = 1+n, n
				if e := tx.Model(&store.JobInstance{}).Where("id = ?", inst.ID).
					Updates(map[string]any{"total_task_num": 1 + n, "running_times": n}).Error; e != nil {
					return struct{}{}, e
				}
			}
		}
		return struct{}{}, nil
	})
	if err == nil && !skipped && noWorker {
		log.Printf("job %d: app %s has no online worker, instance %d failed fast", job.ID, job.AppId, instanceId)
	}
	if err == nil && !skipped && !noWorker {
		s.notify()
	}
	return skipped, instanceId, err
}

// TriggerManually 供 API 手动触发一次（不等 cron，不推进下次触发时间）。
func (s *Scheduler) TriggerManually(job *store.JobInfo) error {
	if !job.Enable {
		return ErrJobPaused
	}
	_, _, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	return err
}

// TriggerRun 与兼容协议的 runJob 语义一致：带运行时参数触发一次，返回新实例 ID。
func (s *Scheduler) TriggerRun(job *store.JobInfo, instanceParams string) (uint64, error) {
	if !job.Enable {
		return 0, ErrJobPaused
	}
	_, id, err := s.fire(job, job.NextTriggerTime, "API", instanceParams, 0)
	return id, err
}

// TriggerDependency 上游实例进入终态后由 Dispatcher 回调：status 为 SUCCEED 时，
// 把所有"以上游为依赖"且已启用的下游任务各触发一次（单上游、成功即触发，失败/停止不传导）。
// 幂等靠 (job_id, upstream_instance_id) 唯一索引：上游回报重试或 server 重启后重放都不会重复拉起下游。
func (s *Scheduler) TriggerDependency(upstreamJobId, upstreamInstanceId uint64, status string) {
	if status != protocol.InstanceSucceed {
		return
	}
	var downs []store.JobInfo
	if err := s.st.DB.Where("upstream_job_id = ? AND enable = ?", upstreamJobId, true).
		Order("id asc").Find(&downs).Error; err != nil {
		log.Printf("dependency trigger: find downstream of job %d: %v", upstreamJobId, err)
		return
	}
	for i := range downs {
		down := downs[i]
		var dup int64
		if err := s.st.DB.Model(&store.JobInstance{}).
			Where("job_id = ? AND upstream_instance_id = ?", down.ID, upstreamInstanceId).
			Count(&dup).Error; err != nil {
			log.Printf("dependency trigger: check duplicates: %v", err)
			continue
		}
		if dup > 0 {
			continue // 已触发过（回报重放）
		}
		// 依赖型任务通常配成 API 类型：不推进 next_trigger_time，只按上游节奏跑
		skipped, id, err := s.fire(&down, down.NextTriggerTime, protocol.SourceDependency, "", upstreamInstanceId)
		switch {
		case err != nil:
			log.Printf("dependency trigger: fire downstream job %d of instance %d: %v", down.ID, upstreamInstanceId, err)
		case skipped:
			log.Printf("dependency trigger: downstream job %d skipped（执行中/超实例上限）", down.ID)
		default:
			log.Printf("dependency trigger: instance %d (job %d) -> downstream instance %d (job %d)",
				upstreamInstanceId, upstreamJobId, id, down.ID)
		}
	}
}
