package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// 内存时间轮（Timing Wheel），对齐 PowerJob InstanceTimeWheelService 的两层调度模型：
// 粗扫线程按扫描周期把到期任务装载进轮子，精确到 tickSize 的触发由轮子完成。
// 跨度 = tickSize × slotNum；超出跨度用 rounds 计数降级，因此可承载任意远延时
// （本项目 FIXED_RATE/FIXED_DELAY 上限 120s，10ms×4096≈41s 一圈即可覆盖）。
type wheelTask struct {
	id        uint64
	runAt     time.Time
	rounds    int
	fn        func()
	cancelled atomic.Bool
}

// Wheel 槽位与 cur 由同一把 mu 保护，Add/Cancel 与驱动协程可安全并发。
type Wheel struct {
	tickSize time.Duration
	slotNum  int

	mu       sync.Mutex
	slots    [][]*wheelTask
	cur      int // 驱动协程下一次要触发的槽位
	clock    time.Time
	handlers map[uint64]*wheelTask

	stopCh chan struct{}
}

func NewWheel(tickSize time.Duration, slotNum int) *Wheel {
	return &Wheel{
		tickSize: tickSize,
		slotNum:  slotNum,
		slots:    make([][]*wheelTask, slotNum),
		clock:    time.Now(),
		handlers: make(map[uint64]*wheelTask),
		stopCh:   make(chan struct{}),
	}
}

// Start 启动驱动：每 tickSize 触发一格，任务异步执行以免阻塞轮子推进。
//
// 必须按"真实流逝了多少个 tick"来推进，而不是"收到一次 tick 事件就走一格"：
// time.Ticker 的 channel 容量只有 1，处理不过来时会把中间的 tick 直接合并丢掉，
// 那种实现下轮子的逻辑时间会永久落后于墙上时间——一次 100ms 的 GC/调度停顿，
// 之后所有亚秒级任务都整体晚 100ms 触发（实测中位延迟 120ms）。改成追赶后，
// 停顿只会让到期任务一次性补触发，稳态精度回到一个 tick 以内。
func (w *Wheel) Start(ctx context.Context) {
	go func() {
		w.mu.Lock()
		w.clock = time.Now()
		w.mu.Unlock()
		ticker := time.NewTicker(w.tickSize)
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			case <-ticker.C:
				elapsed := time.Since(last)
				steps := int(elapsed / w.tickSize)
				if steps < 1 {
					steps = 1
				}
				// 保留不足一格的时间余量，避免每轮再攒一点误差
				last = last.Add(time.Duration(steps) * w.tickSize)
				for ; steps > 0; steps-- {
					w.advance()
				}
			}
		}
	}()
}

func (w *Wheel) advance() {
	w.mu.Lock()
	idx := w.cur
	due := w.slots[idx]
	w.slots[idx] = nil
	w.cur = (idx + 1) % w.slotNum
	w.clock = w.clock.Add(w.tickSize)

	ready := make([]*wheelTask, 0, len(due))
	var again []*wheelTask
	for _, t := range due {
		switch {
		case t.cancelled.Load():
		case t.rounds > 0:
			t.rounds--
			again = append(again, t)
		default:
			ready = append(ready, t)
			delete(w.handlers, t.id)
		}
	}
	w.slots[idx] = again
	w.mu.Unlock()

	for _, t := range ready {
		go t.fn()
	}
}

// Add 注册一次性延时任务；同 id 已存在则先撤销再挂（相当于重新装载）。
// runAt 落在过去则视为立即触发。
func (w *Wheel) Add(id uint64, runAt time.Time, fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 格数必须按**逻辑时钟**换算，不能按 time.Until(真实 now)：
	// 驱动协程因 GC/调度停顿而追赶时，cur 对应的时刻比真实时间早 1.5s，
	// 这时刚挂的"100ms 后"若按真实时间只算出 1 格，会被追赶循环当场踩掉 →
	// 一个 100ms 任务在停顿后被连发十几次、然后又长时间静默（实测 42s 只触发 91 次）。
	remaining := runAt.Sub(w.clock)
	var steps int64
	if remaining > 0 {
		steps = int64(remaining / w.tickSize)
		if remaining%w.tickSize != 0 {
			steps++ // 向上取整，保证不早于 runAt
		}
	}
	if old, ok := w.handlers[id]; ok {
		old.cancelled.Store(true)
	}
	t := &wheelTask{id: id, runAt: runAt, rounds: int(steps / int64(w.slotNum)), fn: fn}
	idx := (w.cur + int(steps)) % w.slotNum
	w.slots[idx] = append(w.slots[idx], t)
	w.handlers[id] = t
}

// Cancel 撤销 id 的任务；不存在则忽略。
func (w *Wheel) Cancel(id uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.handlers[id]; ok {
		t.cancelled.Store(true)
		delete(w.handlers, id)
	}
}

// Pending 当前挂起任务数（观测与测试用）。
func (w *Wheel) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.handlers)
}

func (w *Wheel) Stop() { close(w.stopCh) }
