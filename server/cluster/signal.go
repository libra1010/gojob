package cluster

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"gojob/server/store"
)

// SignalPollInterval 副本轮询入队信号的周期。
// 注意这不是"从不等"到"最多等 1s"的差别那么简单：长轮询本身已有 ≤5s 的兜底重查
// （见 dispatch.Fetch 的 wait 上限），所以信号把跨副本的新任务可见延迟从最多 5s 压到最多 1s，
// 而不是从 wait_seconds 压到 1s。
const SignalPollInterval = time.Second

// Signal 跨副本的"队列变了"广播：一张单行计数器表，seq 自增即一次广播。
// 为什么不用 pg LISTEN/NOTIFY：那要把三库一致的前提破成"仅 postgres 有这能力"，
// 而这行计数器一条 UPDATE 就够，mysql/sqlite 同样有效。
type Signal struct {
	db *gorm.DB
}

func NewSignal(db *gorm.DB) *Signal { return &Signal{db: db} }

// EnsureRow 建出那唯一一行（幂等）。Migrate 已经建过，这里只兜底手工清库/迁移遗漏。
func (s *Signal) EnsureRow() error {
	now := store.Now()
	return s.db.Where("id = ?", store.SignalRowID).
		FirstOrCreate(&store.ClusterSignal{ID: store.SignalRowID, CreateTime: now, ModifyTime: now}).Error
}

// Bump 广播一次。失败只用于记日志：本机唤醒不依赖它，长轮询本身还有 ≤5s 的兜底重查。
// 一行都不存在时（有人手工清库）自愈重建再试一次，而不是静默丢掉这次广播。
func (s *Signal) Bump() error {
	bump := func() (int64, error) {
		res := s.db.Model(&store.ClusterSignal{}).Where("id = ?", store.SignalRowID).
			Updates(map[string]any{"seq": gorm.Expr("seq + 1"), "modify_time": store.Now()})
		return res.RowsAffected, res.Error
	}
	affected, err := bump()
	if err != nil {
		log.Printf("cluster signal bump: %v", err)
		return err
	}
	if affected > 0 {
		return nil
	}
	if err := s.EnsureRow(); err != nil {
		log.Printf("cluster signal ensure row: %v", err)
		return err
	}
	_, err = bump()
	return err
}

// Seq 当前的信号序号。
func (s *Signal) Seq() (int64, error) {
	var row store.ClusterSignal
	if err := s.db.Where("id = ?", store.SignalRowID).First(&row).Error; err != nil {
		return 0, err
	}
	return row.Seq, nil
}

// Watch 起一个后台循环：发现 seq 变化就调用 onFire（通常是 Dispatcher 的本机唤醒）。
// 首跳只记录基准值，不触发回调，免得进程刚起来就空转一轮认领。
func (s *Signal) Watch(ctx context.Context, onFire func()) {
	s.watch(ctx, SignalPollInterval, onFire)
}

func (s *Signal) watch(ctx context.Context, interval time.Duration, onFire func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	last, err := s.Seq()
	if err != nil {
		log.Printf("cluster signal watch: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur, err := s.Seq()
			if err != nil {
				log.Printf("cluster signal watch: %v", err)
				continue
			}
			if cur != last {
				last = cur
				onFire()
			}
		}
	}
}
