package dispatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

// ---------- 测试脚手架 ----------

// newTree 造一个"一棵任务树"形态的实例：根任务已由 worker-A 认领在跑。
// 与 newTask 的区别是把执行类型、task_key、task_name 都按真实建单路径填齐，
// 否则测的是残缺数据。
func newTree(t *testing.T, st *store.Store, executeType string) (uint64, uint64) {
	t.Helper()
	now := store.Now()
	inst := store.JobInstance{JobId: 1, AppId: "app1", Status: protocol.InstanceRunning,
		ExecuteType: executeType, RunningTimes: 1, TotalTaskNum: 1,
		CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&inst).Error; err != nil {
		t.Fatal(err)
	}
	root := store.InstanceTask{
		JobId: 1, InstanceId: inst.ID, AppId: "app1",
		Processor: "demo", ProcessorType: protocol.ProcessorBuiltIn,
		ExecuteType: executeType, TaskKey: protocol.RootTaskKey, TaskName: protocol.RootTaskName,
		Status: protocol.TaskRunning, WorkerId: "worker-A",
		Attempts: 1, MaxAttempts: 3, LogLevel: "INFO",
		DispatchTime: now, CreateTime: now, ModifyTime: now,
	}
	if err := st.DB.Create(&root).Error; err != nil {
		t.Fatal(err)
	}
	return inst.ID, root.ID
}

func newTreeDispatcher(t *testing.T, st *store.Store, workers ...string) *Dispatcher {
	t.Helper()
	reg := registry.New(st.DB, time.Minute)
	for _, w := range workers {
		if err := reg.Alive(protocol.WorkerInfo{InstanceId: w, AppId: "app1", Address: w + ":7799"}); err != nil {
			t.Fatalf("register %s: %v", w, err)
		}
	}
	return New(st, reg, nil)
}

// mapFrom 模拟处理器调 ctx.Map：payloads 是每个子任务的 JSON 负载。
func mapFrom(t *testing.T, d *Dispatcher, worker string, taskID uint64, batch int,
	taskName string, payloads ...string) (*protocol.MapTaskResp, error) {
	t.Helper()
	return d.MapTasks(&protocol.MapTaskReq{
		InstanceId: worker, AppId: "app1", TaskId: taskID,
		TaskName: taskName, BatchNo: batch, SubTasks: payloads,
	})
}

func mustMap(t *testing.T, d *Dispatcher, worker string, taskID uint64, batch int,
	taskName string, payloads ...string) *protocol.MapTaskResp {
	t.Helper()
	resp, err := mapFrom(t, d, worker, taskID, batch, taskName, payloads...)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	return resp
}

func childrenOf(t *testing.T, st *store.Store, parentID uint64) []store.InstanceTask {
	t.Helper()
	var rows []store.InstanceTask
	if err := st.DB.Where("parent_task_id = ?", parentID).Order("id asc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func countRows(t *testing.T, st *store.Store, instanceID uint64, name string) int64 {
	t.Helper()
	var n int64
	if err := st.DB.Model(&store.InstanceTask{}).
		Where("instance_id = ? AND task_name = ?", instanceID, name).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// claimAs 让某台 worker 认领一轮，返回它实际拿到的任务。
func claimAs(t *testing.T, d *Dispatcher, worker string, max int) []protocol.TaskDTO {
	t.Helper()
	tasks, err := d.claim(&protocol.FetchTaskReq{InstanceId: worker, AppId: "app1"}, max)
	if err != nil {
		t.Fatalf("claim by %s: %v", worker, err)
	}
	return tasks
}

// adoptAll 把队列里所有待认领的任务交给一台 worker（走真实认领路径）。
// 子任务是刚建出来的 WAITING 行，而 Report 只认归属 worker，
// 不先认领就回报会被当成越权回报静默忽略——测试里踩过一次，所以显式建这一步。
func adoptAll(t *testing.T, d *Dispatcher, worker string) int {
	t.Helper()
	return len(claimAs(t, d, worker, 100))
}

func reportResult(t *testing.T, d *Dispatcher, worker string, taskID uint64, success bool, result string) {
	t.Helper()
	status := protocol.TaskSucceed
	if !success {
		status = protocol.TaskFailed
	}
	if err := d.Report(t.Context(), &protocol.ReportTaskReq{
		InstanceId: worker, AppId: "app1", TaskId: taskID,
		Success: success, Status: status, Result: result,
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
}

// ---------- map 派生与收敛 ----------

// 根任务 map 出子任务后自己先结束，实例不能提前收敛——这是"一实例一棵树"最容易踩的地方。
func TestMapChildrenKeepInstanceAliveAfterRootFinishes(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMap)

	resp := mustMap(t, d, "worker-A", rootID, 1, "SPLIT", `{"i":1}`, `{"i":2}`, `{"i":3}`)
	adoptAll(t, d, "worker-A")
	if resp.Created != 3 || len(resp.TaskKeys) != 3 {
		t.Fatalf("map 返回 %+v, want 建 3 个", resp)
	}
	want := []string{"0.1", "0.2", "0.3"}
	for i, k := range want {
		if resp.TaskKeys[i] != k {
			t.Fatalf("task_keys = %v, want %v", resp.TaskKeys, want)
		}
	}
	kids := childrenOf(t, st, rootID)
	if kids[0].SubTaskContent != `{"i":1}` || kids[0].TaskName != "SPLIT" {
		t.Fatalf("子任务负载/名字未落库：%+v", kids[0])
	}
	if kids[0].MaxAttempts != 3 {
		t.Fatalf("子任务未继承父任务的重试额度：max_attempts=%d", kids[0].MaxAttempts)
	}
	if c := getInst(t, st, instID); c.TotalTaskNum != 4 || c.RunningTimes != 4 {
		t.Fatalf("实例计数 total=%d running=%d, want 4/4（根任务还在跑）", c.TotalTaskNum, c.RunningTimes)
	}

	// 根任务先回报成功：三个子任务还没跑，实例必须仍在运行
	reportResult(t, d, "worker-A", rootID, true, "已切分 3 片")
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("根任务一结束实例就收敛了：%s", c.Status)
	}

	// 逐个收尾：前两个不收敛，最后一个才收敛
	for i, k := range kids {
		reportResult(t, d, "worker-A", k.ID, true, fmt.Sprintf("r%d", i+1))
		if i < 2 {
			if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
				t.Fatalf("第 %d 个子任务收尾后实例变成 %s, want 仍 RUNNING", i+1, c.Status)
			}
			continue
		}
		c := getInst(t, st, instID)
		if c.Status != protocol.InstanceSucceed {
			t.Fatalf("全部子任务收尾后实例 = %s, want SUCCEED", c.Status)
		}
		// 非 MAP_REDUCE 的实例结果取根任务的返回值（兼容协议同语义）
		if c.Result != "已切分 3 片" {
			t.Fatalf("实例结果 = %q, want 根任务的结果", c.Result)
		}
	}
}

// HTTP 重放同一个批次不能二次派生：一次网络超时就等于双份执行是不可接受的。
func TestMapBatchReplayIsIdempotent(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	_, rootID := newTree(t, st, protocol.ExecuteMap)

	first := mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	again, err := mapFrom(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	if err != nil {
		t.Fatalf("重放同一批不应报错: %v", err)
	}
	if again.Created != 0 {
		t.Fatalf("重放又建了 %d 条，幂等键没生效", again.Created)
	}
	if fmt.Sprint(again.TaskKeys) != fmt.Sprint(first.TaskKeys) {
		t.Fatalf("重放回去了另一批任务：%v vs %v", again.TaskKeys, first.TaskKeys)
	}
	if n := len(childrenOf(t, st, rootID)); n != 2 {
		t.Fatalf("子任务行数 = %d, want 2", n)
	}
	// 第二次调用 map（批次号递增）是正常用法，必须真的建出来
	third := mustMap(t, d, "worker-A", rootID, 2, "SPLIT2", "c")
	if third.Created != 1 || third.TaskKeys[0] != "0.3" {
		t.Fatalf("第二批 map = %+v, want 新建 1 条且序号续上", third)
	}
}

// 越界与非法调用必须当场拒绝，而不是留下半棵树。
func TestMapRejectsIllegalCalls(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A", "worker-B")

	_, rootID := newTree(t, st, protocol.ExecuteStandalone)
	if _, err := mapFrom(t, d, "worker-A", rootID, 1, "SPLIT", "a"); err == nil ||
		!strings.Contains(err.Error(), "STANDALONE") {
		t.Fatalf("STANDALONE 里 map 应被拒绝并点名执行类型，实际 err=%v", err)
	}
	if len(childrenOf(t, st, rootID)) != 0 {
		t.Fatal("被拒绝的 map 仍然落了子任务")
	}

	_, rootID = newTree(t, st, protocol.ExecuteMap)
	if _, err := mapFrom(t, d, "worker-B", rootID, 1, "SPLIT", "a"); err == nil ||
		!strings.Contains(err.Error(), "rejected") {
		t.Fatalf("非归属 worker 替别人 map 应被拒绝，实际 err=%v", err)
	}
	for _, name := range []string{protocol.RootTaskName, protocol.LastTaskName, protocol.BroadcastTaskName} {
		if _, err := mapFrom(t, d, "worker-A", rootID, 1, name, "a"); err == nil ||
			!strings.Contains(err.Error(), "reserved") {
			t.Fatalf("保留名 %s 应被拒绝，实际 err=%v", name, err)
		}
	}
	if _, err := mapFrom(t, d, "worker-A", rootID, 1, ""); err == nil {
		t.Fatal("空 taskName 应被拒绝")
	}
	if _, err := mapFrom(t, d, "worker-A", rootID, 1, "SPLIT"); err == nil {
		t.Fatal("空子任务列表应被拒绝")
	}
	// 根任务已终态后不能再派生（否则收敛判定会被事后加的行推翻）
	reportResult(t, d, "worker-A", rootID, true, "done")
	if _, err := mapFrom(t, d, "worker-A", rootID, 1, "SPLIT", "a"); err == nil ||
		!strings.Contains(err.Error(), "running task") {
		t.Fatalf("已结束的任务不能再 map，实际 err=%v", err)
	}
}

// 多级 map：子任务再派生孙任务，孙任务没跑完时实例不许收敛。
func TestMultiLevelMap(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMap)

	mustMap(t, d, "worker-A", rootID, 1, "L1", "a", "b")
	l1 := childrenOf(t, st, rootID)
	adoptAll(t, d, "worker-A")
	reportResult(t, d, "worker-A", rootID, true, "root")

	mustMap(t, d, "worker-A", l1[0].ID, 1, "L2", "a1", "a2")
	gk := childrenOf(t, st, l1[0].ID)
	if len(gk) != 2 || gk[0].TaskKey != "0.1.1" {
		t.Fatalf("孙任务 %+v, want 2 条且 task_key 从 0.1.1 起", gk)
	}
	adoptAll(t, d, "worker-A")
	// 第一个 L1 自己结束，但它的两个孙任务还在
	reportResult(t, d, "worker-A", l1[0].ID, true, "l1-1")
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("孙任务未收敛时实例提前结束：%s", c.Status)
	}
	for _, g := range gk {
		reportResult(t, d, "worker-A", g.ID, true, "leaf")
	}
	reportResult(t, d, "worker-A", l1[1].ID, true, "l1-2")
	if c := getInst(t, st, instID); c.Status != protocol.InstanceSucceed {
		t.Fatalf("全树收敛后实例 = %s, want SUCCEED", c.Status)
	}
	if c := getInst(t, st, instID); c.TotalTaskNum != 5 {
		t.Fatalf("total_task_num = %d, want 5（根+2 子+2 孙）", c.TotalTaskNum)
	}
}

// MAP 型：任一子任务最终失败即实例失败，失败原因留在失败的那一行里。
func TestMapChildFailureFailsInstance(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMap)
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	kids := childrenOf(t, st, rootID)
	adoptAll(t, d, "worker-A")
	reportResult(t, d, "worker-A", rootID, true, "root")

	reportResult(t, d, "worker-A", kids[0].ID, true, "ok")
	reportResult(t, d, "worker-A", kids[1].ID, false, "boom")
	// max_attempts=3、attempts=1：第一次失败应当重排队而不是定局
	if k := getTask(t, st, kids[1].ID); k.Status != protocol.TaskWaiting {
		t.Fatalf("失败的子任务 status = %s, want 重回 WAITING 重试", k.Status)
	}
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("重试期间实例不该收敛：%s", c.Status)
	}
	// 第 2、3 次失败：每次重排队都会清空归属，必须重新认领才轮得到再回报
	for i := 2; i <= 3; i++ {
		adoptAll(t, d, "worker-A")
		reportResult(t, d, "worker-A", kids[1].ID, false, "boom again")
		if i < 3 {
			if k := getTask(t, st, kids[1].ID); k.Status != protocol.TaskWaiting {
				t.Fatalf("第 %d 次失败后 status = %s, want 仍重回 WAITING", i, k.Status)
			}
		}
	}
	c := getInst(t, st, instID)
	if c.Status != protocol.InstanceFailed {
		t.Fatalf("子任务重试耗尽后实例 = %s, want FAILED", c.Status)
	}
	if c.FailedTaskNum != 1 {
		t.Fatalf("failed_task_num = %d, want 1", c.FailedTaskNum)
	}
}

// ---------- MAP_REDUCE ----------

// reduce 任务在整棵树排空时才出现，并且它的返回值决定实例终态与结果。
func TestMapReduceLastTaskRunsAfterDrain(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMapReduce)
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	kids := childrenOf(t, st, rootID)
	adoptAll(t, d, "worker-A")
	reportResult(t, d, "worker-A", rootID, true, "root")

	if n := countRows(t, st, instID, protocol.LastTaskName); n != 0 {
		t.Fatalf("子任务还没跑完就有了 reduce 行（%d 条）", n)
	}
	reportResult(t, d, "worker-A", kids[0].ID, true, "1")
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("还差一个子任务就收敛了：%s", c.Status)
	}
	reportResult(t, d, "worker-A", kids[1].ID, true, "2")

	last := countRows(t, st, instID, protocol.LastTaskName)
	if last != 1 {
		t.Fatalf("reduce 行数 = %d, want 1", last)
	}
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("reduce 还没跑，实例就该是 RUNNING，实际 %s", c.Status)
	}
	var reduceTask store.InstanceTask
	if err := st.DB.Where("instance_id = ? AND task_name = ?", instID, protocol.LastTaskName).
		First(&reduceTask).Error; err != nil {
		t.Fatal(err)
	}
	if reduceTask.TaskKey != protocol.LastTaskKey || reduceTask.Processor != "demo" {
		t.Fatalf("reduce 任务 %+v, want task_key=9999 且沿用根任务的处理器", reduceTask)
	}
	if reduceTask.TargetWorkerId != "" {
		t.Fatalf("reduce 不该钉死在某台机器上（结果在库里，钉住只会多一个失败模式），实际 %q",
			reduceTask.TargetWorkerId)
	}

	// reduce 能看到全部子任务结果（先认领：取结果同样要求归属本 worker）
	adoptAll(t, d, "worker-A")
	got, err := d.TaskResults(&protocol.TaskResultsReq{
		InstanceId: "worker-A", AppId: "app1", TaskId: reduceTask.ID})
	if err != nil {
		t.Fatalf("taskResults: %v", err)
	}
	if got.Total != 2 || got.Results[0].Result != "1" || got.Results[1].Result != "2" {
		t.Fatalf("reduce 取到的结果 %+v, want 2 条且顺序稳定", got.Results)
	}
	if !got.Results[0].Success || got.Results[0].TaskName != "SPLIT" {
		t.Fatalf("reduce 结果缺字段：%+v", got.Results[0])
	}

	// reduce 也是个普通任务，失败先走满重试额度才定局（每轮重排队后要重新认领）
	for i := 0; i < 3; i++ {
		adoptAll(t, d, "worker-A")
		reportResult(t, d, "worker-A", reduceTask.ID, false, "reduce 汇总失败")
	}
	c := getInst(t, st, instID)
	if c.Status != protocol.InstanceFailed || c.Result != "reduce 汇总失败" {
		t.Fatalf("reduce 失败后实例 = %s/%q, want FAILED/Reduce 的原因", c.Status, c.Result)
	}
}

// reduce 成功即实例成功——这是 MAP_REDUCE 相对 MAP 的全部意义：容忍部分失败。
func TestMapReduceToleratesPartialChildFailure(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMapReduce)
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	kids := childrenOf(t, st, rootID)
	adoptAll(t, d, "worker-A")
	reportResult(t, d, "worker-A", rootID, true, "root")
	reportResult(t, d, "worker-A", kids[0].ID, true, "1")
	// 把第二个子任务的剩余重试额度用光
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", kids[1].ID).
		Update("attempts", 3).Error; err != nil {
		t.Fatal(err)
	}
	reportResult(t, d, "worker-A", kids[1].ID, false, "boom")
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("reduce 还没跑，实例不该定局：%s", c.Status)
	}
	var reduceTask store.InstanceTask
	if err := st.DB.Where("instance_id = ? AND task_name = ?", instID, protocol.LastTaskName).
		First(&reduceTask).Error; err != nil {
		t.Fatalf("子任务失败后仍应生成 reduce：%v", err)
	}
	adoptAll(t, d, "worker-A")
	reportResult(t, d, "worker-A", reduceTask.ID, true, "1 成功 1 失败，已补偿")
	c := getInst(t, st, instID)
	if c.Status != protocol.InstanceSucceed || c.Result != "1 成功 1 失败，已补偿" {
		t.Fatalf("实例 = %s/%q, want SUCCEED/Reduce 的结论", c.Status, c.Result)
	}
	if c.FailedTaskNum != 1 {
		t.Fatalf("failed_task_num = %d, want 1（失败计数照实保留，只是不再决定终态）", c.FailedTaskNum)
	}
}

// 用户停止过的实例不许在收尾时又冒出一个 reduce 任务。
func TestStoppedInstanceDoesNotSpawnReduceTask(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMapReduce)
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	mustStop(t, d, instID)
	if n := countRows(t, st, instID, protocol.LastTaskName); n != 0 {
		t.Fatalf("停止后仍生成了 %d 个 reduce 任务", n)
	}
	// 未认领的子任务当场 STOPPED，根任务还在 worker 手上 → STOPPING
	if c := getInst(t, st, instID); c.Status != protocol.InstanceStopping {
		t.Fatalf("根任务未收尾前实例 = %s, want STOPPING", c.Status)
	}
	// 根任务协作退出后收敛为 STOPPED，此时树已排空——这一步才是"不许补 reduce"的关键
	reportResult(t, d, "worker-A", rootID, false, "cancelled")
	if n := countRows(t, st, instID, protocol.LastTaskName); n != 0 {
		t.Fatalf("停止过的实例收尾时又补出了 %d 个 reduce 任务", n)
	}
	if c := getInst(t, st, instID); c.Status != protocol.InstanceStopped {
		t.Fatalf("实例 = %s, want STOPPED", c.Status)
	}
}

// ---------- 重派与失联 ----------

// 父任务重跑会重新 map，旧的未收敛后代必须作废，否则同一份工作被执行两遍。
func TestRedispatchDiscardsPendingDescendants(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMap)
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	kids := childrenOf(t, st, rootID)
	adoptAll(t, d, "worker-A")
	// 一个已经跑完（既成事实，保留），一个还在排队
	reportResult(t, d, "worker-A", kids[0].ID, true, "1")

	reportResult(t, d, "worker-A", rootID, false, "根任务失败，重跑")
	if k := getTask(t, st, rootID); k.Status != protocol.TaskWaiting {
		t.Fatalf("根任务 status = %s, want 重回 WAITING", k.Status)
	}
	if k := getTask(t, st, kids[1].ID); k.Status != protocol.TaskDiscarded {
		t.Fatalf("未收敛的兄弟任务 status = %s, want DISCARDED", k.Status)
	}
	if k := getTask(t, st, kids[0].ID); k.Status != protocol.TaskSucceed {
		t.Fatalf("已成功的子任务被追溯改判成 %s", k.Status)
	}
	if c := getInst(t, st, instID); c.Status != protocol.InstanceRunning {
		t.Fatalf("作废不该让实例收敛：%s", c.Status)
	}
	// 重跑后同一批次号必须真的建新任务，而不是被上一批的作废行"幂等"掉
	adoptAll(t, d, "worker-A") // 重排队后根任务要重新被认领，才能再 map
	again := mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a", "b")
	if again.Created != 2 {
		t.Fatalf("重跑后 map 只建了 %d 条，作废行挡住了新批次", again.Created)
	}
	if again.TaskKeys[0] != "0.3" {
		t.Fatalf("重跑后的 task_key = %v, want 序号继续往后排不与作废行重号", again.TaskKeys)
	}
}

// 广播子任务的 target_worker_id 就是"每台一份"这件事本身：
// 失联不重派（判失败），失败重试也留在原机上，回收预选时也不能放出去。
func TestBroadcastChildStaysOnItsPinnedWorker(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A", "worker-B")
	instID, _ := newTree(t, st, protocol.ExecuteBroadcast)
	var root store.InstanceTask
	if err := st.DB.Where("instance_id = ? AND task_name = ?", instID, protocol.RootTaskName).
		First(&root).Error; err != nil {
		t.Fatal(err)
	}
	// 广播的根任务不派给任何 worker：fire() 建出来就是终态（它只代表"这一份广播已派下去"）
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", root.ID).
		Updates(map[string]any{"status": protocol.TaskSucceed, "worker_id": "worker-A",
			"result": "广播 2 个执行节点"}).Error; err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	pinned := map[string]uint64{}
	for _, w := range []string{"worker-A", "worker-B"} {
		kid := store.InstanceTask{
			JobId: 1, InstanceId: instID, AppId: "app1", Processor: "demo",
			ExecuteType: protocol.ExecuteBroadcast, ParentTaskId: root.ID,
			TaskKey: "0." + w, TaskName: protocol.BroadcastTaskName,
			Status: protocol.TaskRunning, WorkerId: w, TargetWorkerId: w,
			Attempts: 1, MaxAttempts: 3, DispatchTime: now.Add(-time.Hour),
			CreateTime: now, ModifyTime: now,
		}
		if err := st.DB.Create(&kid).Error; err != nil {
			t.Fatal(err)
		}
		pinned[w] = kid.ID
	}

	// 两台都心跳停摆才算"失联回收"场景：节点还活着时在跑的任务不能被重派。
	if err := st.DB.Model(&store.WorkerNode{}).Where("app_id = ?", "app1").
		Update("last_heartbeat", store.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	// 1) 失联回收：广播子任务不重派，直接判失败
	if err := d.ReassignLostTasks(time.Minute); err != nil {
		t.Fatalf("ReassignLostTasks: %v", err)
	}
	for w, id := range pinned {
		if k := getTask(t, st, id); k.Status != protocol.TaskFailed {
			t.Fatalf("%s 上的广播子任务 status = %s, want FAILED（换机重跑等于多干一份）",
				w, k.Status)
		}
	}
	if c := getInst(t, st, instID); c.Status != protocol.InstanceFailed {
		t.Fatalf("两份广播都失败后实例 = %s, want FAILED", c.Status)
	}
	// 回收完成后节点重新上线，后续认领才能通过存活校验
	for _, w := range []string{"worker-A", "worker-B"} {
		if err := d.reg.Alive(protocol.WorkerInfo{InstanceId: w, AppId: "app1", Address: w + ":7799"}); err != nil {
			t.Fatal(err)
		}
	}

	// 2) 失败重试保留指名：重排队的行仍然只属于原来那台机器
	st.DB.Model(&store.JobInstance{}).Where("id = ?", instID).
		Update("status", protocol.InstanceRunning)
	kid := store.InstanceTask{
		JobId: 1, InstanceId: instID, AppId: "app1", Processor: "demo",
		ExecuteType: protocol.ExecuteBroadcast, ParentTaskId: root.ID,
		TaskKey: "0.retry", TaskName: protocol.BroadcastTaskName,
		Status: protocol.TaskRunning, WorkerId: "worker-A", TargetWorkerId: "worker-A",
		Attempts: 1, MaxAttempts: 3, DispatchTime: now, CreateTime: now, ModifyTime: now,
	}
	if err := st.DB.Create(&kid).Error; err != nil {
		t.Fatal(err)
	}
	reportResult(t, d, "worker-A", kid.ID, false, "boom")
	if k := getTask(t, st, kid.ID); k.Status != protocol.TaskWaiting || k.TargetWorkerId != "worker-A" {
		t.Fatalf("广播子任务重试丢了指名：status=%s target=%q", k.Status, k.TargetWorkerId)
	}
	// 别的机器认领不到它
	if got := claimAs(t, d, "worker-B", 5); len(got) != 0 {
		t.Fatalf("worker-B 领到了指名给 worker-A 的广播子任务：%+v", got)
	}
	if got := claimAs(t, d, "worker-A", 5); len(got) != 1 || got[0].TaskName != protocol.BroadcastTaskName {
		t.Fatalf("worker-A 应恰好领到自己那一份，实际 %+v", got)
	}
	// 3) 超时未取件的回收也不能把它放出去
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", kid.ID).
		Updates(map[string]any{"status": protocol.TaskWaiting, "dispatch_time": now.Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.ReleaseStaleTargets(time.Minute); err != nil {
		t.Fatal(err)
	}
	if k := getTask(t, st, kid.ID); k.TargetWorkerId != "worker-A" {
		t.Fatalf("ReleaseStaleTargets 把广播子任务放进了自由池：target=%q", k.TargetWorkerId)
	}
}

// ---------- 认领下发的字段 ----------

// worker 要靠 TaskKey/TaskName/SubTaskContent/ExecuteType 才能分辨"我在跑第几片、能不能再 map"。
func TestClaimCarriesSubTaskFields(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	_, rootID := newTree(t, st, protocol.ExecuteMap)
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", `{"i":7}`)

	got := claimAs(t, d, "worker-A", 5)
	if len(got) != 1 {
		t.Fatalf("认领到 %d 条，want 1", len(got))
	}
	t0 := got[0]
	if t0.TaskKey != "0.1" || t0.TaskName != "SPLIT" || t0.ExecuteType != protocol.ExecuteMap ||
		t0.SubTaskContent != `{"i":7}` {
		t.Fatalf("下发字段不完整：%+v", t0)
	}
}

// 结果长度必须裁：一个失控的 println 不该把库撑爆。
func TestReportTruncatesResult(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	_, rootID := newTree(t, st, protocol.ExecuteMap)
	long := strings.Repeat("x", maxResultLength*2)
	reportResult(t, d, "worker-A", rootID, true, long)
	if k := getTask(t, st, rootID); len(k.Result) != maxResultLength+len("...(结果已截断)") {
		t.Fatalf("结果长度 = %d, want %d（截断标记在内）", len(k.Result), maxResultLength)
	}
}

// taskResults 只能由 reduce 任务、且只能由正在执行它的 worker 来取。
func TestTaskResultsRequiresReduceTask(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMapReduce)
	if _, err := d.TaskResults(&protocol.TaskResultsReq{
		InstanceId: "worker-A", AppId: "app1", TaskId: rootID}); err == nil ||
		!strings.Contains(err.Error(), "not a reduce task") {
		t.Fatalf("根任务来取结果应被拒绝，实际 err=%v", err)
	}
	mustMap(t, d, "worker-A", rootID, 1, "SPLIT", "a")
	kids := childrenOf(t, st, rootID)
	adoptAll(t, d, "worker-A")
	reportResult(t, d, "worker-A", rootID, true, "root")
	reportResult(t, d, "worker-A", kids[0].ID, true, "1")
	var reduceTask store.InstanceTask
	if err := st.DB.Where("instance_id = ? AND task_name = ?", instID, protocol.LastTaskName).
		First(&reduceTask).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := d.TaskResults(&protocol.TaskResultsReq{
		InstanceId: "worker-B", AppId: "app1", TaskId: reduceTask.ID}); err == nil {
		t.Fatal("非归属 worker 也能取 reduce 结果")
	}
}
