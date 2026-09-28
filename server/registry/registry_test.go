// 接单开关的库侧语义：停接单的节点要从分发候选集里消失，而且**心跳不能把它刷回来**。
// 后者是这次改动最容易踩的坑——在线表是每 5s upsert 一次的，
// 如果 accept_tasks 跟着进了覆盖列，运维刚点的"停止接单"会在下一跳被 worker 自己抹掉。
package registry

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/store"
)

func newReg(t *testing.T) (*Registry, *store.Store) {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return New(st.DB, 15*time.Second), st
}

func beat(node, instance, address string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		InstanceId: instance, AppId: node, Address: address,
		Version: "test", SdkType: "GO", RunningTasks: 1,
	}
}

// instanceIDs 取候选集里的实例号，失败信息里能直接看出少了谁。
func instanceIDs(ws []protocol.WorkerInfo) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.InstanceId)
	}
	return out
}

func TestDrainedNodeLeavesDispatchSetAndSurvivesHeartbeats(t *testing.T) {
	r, _ := newReg(t)
	for i, id := range []string{"w-1", "w-2", "w-3"} {
		if err := r.Alive(beat("app1", id, fmt.Sprintf("10.0.0.%d:7799", i+1))); err != nil {
			t.Fatalf("heartbeat %s: %v", id, err)
		}
	}
	if got := len(r.Workers("app1")); got != 3 {
		t.Fatalf("候选节点 = %d, want 3", got)
	}

	n, err := r.SetAccept("app1", "w-2", false)
	if err != nil || n != 1 {
		t.Fatalf("SetAccept = (%d, %v), want (1, nil)", n, err)
	}

	got := instanceIDs(r.Workers("app1"))
	if len(got) != 2 {
		t.Fatalf("停接单后候选集 = %v, want 2 台", got)
	}
	for _, id := range got {
		if id == "w-2" {
			t.Fatal("已停止接单的节点还在分发候选集里")
		}
	}

	// 关键回归：那台节点还在正常心跳（它不知道自己被停了，也不需要知道），
	// 但它的每一跳都不能把管理面的开关刷回接单中。
	for i := 0; i < 3; i++ {
		if err := r.Alive(beat("app1", "w-2", "10.0.0.2:7799")); err != nil {
			t.Fatal(err)
		}
	}
	if len(instanceIDs(r.Workers("app1"))) != 2 {
		t.Fatal("心跳把停止接单刷回去了")
	}

	// 监控面必须还看得见它，否则开关点不回来。
	nodes := r.AliveNodes("app1")
	if len(nodes) != 3 {
		t.Fatalf("AliveNodes = %d, want 3（含已停止接单的）", len(nodes))
	}
	for _, nd := range nodes {
		switch nd.InstanceId {
		case "w-2":
			if nd.AcceptTasks {
				t.Fatal("w-2 应显示为已停止接单")
			}
		default:
			if !nd.AcceptTasks {
				t.Fatalf("%s 应显示为接单中", nd.InstanceId)
			}
		}
	}

	// 恢复：立刻回到候选集。
	if n, err := r.SetAccept("app1", "w-2", true); err != nil || n != 1 {
		t.Fatalf("恢复接单 = (%d, %v)", n, err)
	}
	if len(instanceIDs(r.Workers("app1"))) != 3 {
		t.Fatal("恢复接单后没回到候选集")
	}
}

// 全部节点都停接单时，任务应当留在队列里等运维恢复，而不是被判成"没有 worker"直接失败：
// HasWorkers 数的是在线节点，不看开关。
func TestHasWorkersIgnoresAcceptSwitch(t *testing.T) {
	r, _ := newReg(t)
	if err := r.Alive(beat("app1", "w-1", "10.0.0.1:7799")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetAccept("app1", "w-1", false); err != nil {
		t.Fatal(err)
	}
	if len(r.Workers("app1")) != 0 {
		t.Fatal("候选集应为空")
	}
	ok, err := r.HasWorkers("app1")
	if err != nil || !ok {
		t.Fatalf("HasWorkers = (%v, %v), want (true, nil)——节点还在线，只是不收单", ok, err)
	}
}

func TestSetAcceptOnUnknownNodeAffectsNoRows(t *testing.T) {
	r, _ := newReg(t)
	if n, err := r.SetAccept("app1", "ghost", false); err != nil || n != 0 {
		t.Fatalf("不存在的节点 = (%d, %v), want (0, nil)", n, err)
	}
}

// 存量库升级：这一列是后加的，历史行必须被认成"接单中"，
// 否则升级那一刻所有在线节点会一起从候选集里消失（表现是任务突然全派不出去）。
func TestLegacyRowWithoutColumnStillAccepts(t *testing.T) {
	r, st := newReg(t)
	if err := r.Alive(beat("app1", "w-1", "10.0.0.1:7799")); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Exec("UPDATE worker_nodes SET accept_tasks = NULL").Error; err != nil {
		t.Fatal(err)
	}
	if len(instanceIDs(r.Workers("app1"))) != 1 {
		t.Fatal("accept_tasks 为 NULL 的存量行被当成停止接单了")
	}
}
