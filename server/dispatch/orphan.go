// 排队任务收尾：两类永远不会有人执行、却会把实例一直挂住的 WAITING 行。
package dispatch

import (
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// waitingSweepBatch 单次巡检最多处理多少行，避免一次长事务卡住认领热路径。
const waitingSweepBatch = 500

// FailOrphanedWaiting 收尾两类排队任务：
//  1. 停止意图已经落下、任务却还在 WAITING 的（停止与失联回收的竞态遗留）——按 STOPPED 收尾；
//  2. 排队超过 grace、且所在项目一个在线节点都没有的——按 FAILED 收尾。
//
// 这是"无节点触发当场失败"的兜底，覆盖触发后节点立刻掉线、以及停止竞态里残留的未派发任务。
// 全停接单的项目仍然排队：HasWorkers 刻意不看 accept_tasks，对应"停接单=暂停派发"的既有语义。
func (d *Dispatcher) FailOrphanedWaiting(grace time.Duration) error {
	stopped, err := d.stopRequestedWaiting()
	if err != nil {
		return err
	}
	noWorker, err := d.failWaitingWithoutWorkers(grace)
	if err != nil {
		return err
	}
	if stopped+noWorker > 0 {
		log.Printf("maintain: closed %d stopped and %d no-worker waiting task(s)", stopped, noWorker)
	}
	return nil
}

// stopRequestedWaiting 停止指令已经落在排队任务上的，一律不再派发。
func (d *Dispatcher) stopRequestedWaiting() (int, error) {
	var lastID uint64
	closed := 0
	for {
		var ids []uint64
		if err := d.st.DB.Model(&store.InstanceTask{}).
			Where("status = ? AND stop_request_time IS NOT NULL AND id > ?",
				protocol.TaskWaiting, lastID).
			Order("id asc").Limit(waitingSweepBatch).Pluck("id", &ids).Error; err != nil {
			return closed, err
		}
		if len(ids) == 0 {
			return closed, nil
		}
		lastID = ids[len(ids)-1]
		for _, id := range ids {
			ok, e := d.settleWaitingTask(id, func(*store.InstanceTask) (string, string) {
				return protocol.TaskStopped, "停止指令已下达，未派发"
			}, time.Time{})
			if e != nil {
				log.Printf("close stopped waiting task %d: %v", id, e)
				continue
			}
			if ok {
				closed++
			}
		}
	}
}

// failWaitingWithoutWorkers 只对"项目已无在线节点"的项目动手；有节点的项目保持排队，
// 让 worker 按空闲额度认领（等价于兼容协议的"过载不派发、等巡检重试"）。
func (d *Dispatcher) failWaitingWithoutWorkers(grace time.Duration) (int, error) {
	cutoff := store.Now().Add(-grace)
	var apps []string
	if err := d.st.DB.Model(&store.InstanceTask{}).
		Where("status = ? AND dispatch_time < ?", protocol.TaskWaiting, cutoff).
		Distinct().Pluck("app_id", &apps).Error; err != nil {
		return 0, err
	}
	result := fmt.Sprintf("无可用 worker：排队超过 %d 秒仍无在线节点", int(grace.Seconds()))
	failed := 0
	for _, appId := range apps {
		ok, err := d.reg.HasWorkers(appId)
		if err != nil {
			log.Printf("probe workers of app %s: %v", appId, err)
			continue
		}
		if ok {
			continue
		}
		n, err := d.failWaitingOfApp(appId, cutoff, result)
		failed += n
		if err != nil {
			log.Printf("fail waiting tasks of app %s: %v", appId, err)
		}
	}
	return failed, nil
}

func (d *Dispatcher) failWaitingOfApp(appId string, cutoff time.Time, result string) (int, error) {
	var lastID uint64
	failed := 0
	for {
		var ids []uint64
		if err := d.st.DB.Model(&store.InstanceTask{}).
			Where("app_id = ? AND status = ? AND dispatch_time < ? AND id > ?",
				appId, protocol.TaskWaiting, cutoff, lastID).
			Order("id asc").Limit(waitingSweepBatch).Pluck("id", &ids).Error; err != nil {
			return failed, err
		}
		if len(ids) == 0 {
			return failed, nil
		}
		lastID = ids[len(ids)-1]
		for _, id := range ids {
			ok, e := d.settleWaitingTask(id, func(t *store.InstanceTask) (string, string) {
				if t.StopRequestTime != nil {
					return protocol.TaskStopped, "停止指令已下达，未派发"
				}
				return protocol.TaskFailed, result
			}, cutoff)
			if e != nil {
				log.Printf("fail waiting task %d: %v", id, e)
				continue
			}
			if ok {
				failed++
			}
		}
	}
}

// settleWaitingTask 在事务内确认任务仍是可以收尾的 WAITING 行，再推进终态并收敛实例。
// cutoff 非零时要求 dispatch_time 仍早于它：查询与更新之间被重试刷新过的任务不能误杀。
func (d *Dispatcher) settleWaitingTask(id uint64, decide func(*store.InstanceTask) (string, string), cutoff time.Time) (bool, error) {
	now := store.Now()
	changed := false
	_, err := d.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		var t store.InstanceTask
		if err := tx.First(&t, id).Error; err != nil {
			return struct{}{}, err
		}
		if t.Status != protocol.TaskWaiting {
			return struct{}{}, nil
		}
		if !cutoff.IsZero() && !t.DispatchTime.Before(cutoff) {
			return struct{}{}, nil
		}
		status, result := decide(&t)
		res := tx.Model(&store.InstanceTask{}).
			Where("id = ? AND status = ?", id, protocol.TaskWaiting).
			Updates(map[string]any{"status": status, "result": result,
				"stop_request_time": nil, "modify_time": now})
		if res.Error != nil {
			return struct{}{}, res.Error
		}
		if res.RowsAffected != 1 {
			return struct{}{}, nil
		}
		changed = true
		t.Status = status
		_, err := finishInstance(tx, &t, status, now)
		return struct{}{}, err
	})
	return changed, err
}
