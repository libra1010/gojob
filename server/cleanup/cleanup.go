// Package cleanup 回收历史实例：终态且已超过保留期的实例，连同它的子任务与任务日志一起删掉。
//
// 为什么必须有：实例与任务日志只增不减。`job_infos.life_cycle` 这个字段从一期就建了，但
// 直到本包之前全项目零处读取——开发库堆到 1.5 万+ 条实例就是这么来的。调度中心是长期进程，
// 没有这道闸门就等于把"哪天表大到拖慢热路径认领"留给运维现场发现。
package cleanup

import (
	"context"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// Interval 是两轮扫描的间隔。清理不需要准时，需要的是"每次少删一点"——把删除摊成很多小批，
// 比一次删空更稳：sqlite 是单写者，MySQL 上的长事务会卡住热路径的认领查询。
const Interval = 10 * time.Minute

const (
	// BatchSize 一批删多少个实例。每批是"20 个实例 + 它们的子任务与日志"，一个短事务。
	BatchSize = 20
	// MaxBatchesPerJob 单个任务单轮最多删多少批。留上限是为了不在一轮里长时间占着连接；
	// 剩下的下一轮接着删，收敛即可，不要求一次做完。
	MaxBatchesPerJob = 25
)

// Runner 按保留期回收历史实例。
type Runner struct {
	st *store.Store
	// owns 该项目的归属是否在本机。多副本下让每个副本只清自己调度过的项目，
	// 免得 N 个副本各拿一份连接去删同一批行（结果是对的，但白做 N 倍工）。
	// nil 表示单机模式：全部都清。
	owns func(appID string) bool
	// defaultDays > 0 时，没配 life_cycle 的任务也按它清（GOJOB_RETENTION_DAYS），
	// 并且顺带回收"任务已删除"的孤儿实例。为 0 时什么都不删——存量部署升级后行为不变。
	defaultDays int
}

func New(st *store.Store, owns func(appID string) bool, defaultDays int) *Runner {
	return &Runner{st: st, owns: owns, defaultDays: defaultDays}
}

// Enabled 供调用方决定要不要起这个循环，也供启动日志如实说明"当前不会清理任何数据"。
func (r *Runner) Enabled() bool { return r.defaultDays > 0 }

// terminalStatuses 只有终态实例会被删——运行中/停止中/等待派发的永不碰。
var terminalStatuses = []string{protocol.InstanceSucceed, protocol.InstanceFailed, protocol.InstanceStopped}

type jobRetention struct {
	JobId     uint64
	AppId     string
	LifeCycle int
}

// Sweep 跑一轮清理，返回删除的实例数。
func (r *Runner) Sweep(ctx context.Context) (int, error) {
	var jobs []jobRetention
	q := r.st.DB.Model(&store.JobInfo{}).Select("id AS job_id", "app_id", "life_cycle")
	if r.defaultDays <= 0 {
		// 没配全局保留天数时，只认任务上显式配置的 life_cycle。
		q = q.Where("life_cycle > 0")
	}
	if err := q.Find(&jobs).Error; err != nil {
		return 0, fmt.Errorf("list jobs: %w", err)
	}

	total := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		days := j.LifeCycle
		if days <= 0 {
			days = r.defaultDays
		}
		if days <= 0 || !r.ownsJob(j.AppId) {
			continue
		}
		n, err := r.purgeJob(ctx, j.JobId, store.Now().AddDate(0, 0, -days))
		if err != nil {
			return total, fmt.Errorf("purge job %d: %w", j.JobId, err)
		}
		total += n
	}
	orphaned, err := r.purgeOrphans(ctx)
	if err != nil {
		return total, fmt.Errorf("purge orphan instances: %w", err)
	}
	return total + orphaned, nil
}

func (r *Runner) ownsJob(appID string) bool { return r.owns == nil || r.owns(appID) }

// purgeJob 删掉某个任务下 cutoff 之前产生的终态实例，分批推进。
func (r *Runner) purgeJob(ctx context.Context, jobID uint64, cutoff time.Time) (int, error) {
	deleted := 0
	for range MaxBatchesPerJob {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		ids, err := r.expiredInstances(jobID, cutoff)
		if err != nil || len(ids) == 0 {
			return deleted, err
		}
		if err := deleteInstances(r.st.DB, ids); err != nil {
			return deleted, err
		}
		deleted += len(ids)
	}
	return deleted, nil
}

func (r *Runner) expiredInstances(jobID uint64, cutoff time.Time) ([]uint64, error) {
	var ids []uint64
	err := r.st.DB.Model(&store.JobInstance{}).
		Where("job_id = ? AND status IN ? AND create_time < ?", jobID, terminalStatuses, cutoff).
		Limit(BatchSize).Pluck("id", &ids).Error
	return ids, err
}

// purgeOrphans 任务被删掉之后，它名下的实例就没有 life_cycle 可依了——这些行既不会被按任务
// 的查询命中，也永远不会被上面的循环清掉，所以按全局保留天数一并回收。
// 只在配了 GOJOB_RETENTION_DAYS 时生效：没配就不擅自删任何数据。
func (r *Runner) purgeOrphans(ctx context.Context) (int, error) {
	if r.defaultDays <= 0 {
		return 0, nil
	}
	cutoff := store.Now().AddDate(0, 0, -r.defaultDays)
	deleted := 0
	for range MaxBatchesPerJob {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		var ids []uint64
		err := r.st.DB.Model(&store.JobInstance{}).
			Where("status IN ? AND create_time < ? AND NOT EXISTS "+
				"(SELECT 1 FROM job_infos j WHERE j.id = job_instances.job_id)",
				terminalStatuses, cutoff).
			Limit(BatchSize).Pluck("id", &ids).Error
		if err != nil || len(ids) == 0 {
			return deleted, err
		}
		if err := deleteInstances(r.st.DB, ids); err != nil {
			return deleted, err
		}
		deleted += len(ids)
	}
	return deleted, nil
}

// deleteInstances 一个短事务里按"日志 → 子任务 → 实例"的顺序删。
// 日志表本身有 instance_id 索引，所以不必绕 subquery 从任务表反查。
func deleteInstances(db *gorm.DB, ids []uint64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("instance_id IN ?", ids).Delete(&store.InstanceTaskLog{}).Error; err != nil {
			return err
		}
		if err := tx.Where("instance_id IN ?", ids).Delete(&store.InstanceTask{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&store.JobInstance{}).Error
	})
}

// Run 是后台循环：每 Interval 扫一次，删除量 > 0 才打日志（空转不刷屏）。
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.Sweep(ctx)
			if err != nil {
				log.Printf("cleanup: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("cleanup: removed %d expired instance(s)", n)
			}
		}
	}
}
