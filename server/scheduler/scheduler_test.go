package scheduler

import (
	"context"
	"sort"
	"testing"
	"time"
)

func TestNextByTypeUnitsAreMilliseconds(t *testing.T) {
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)

	// FIXED_RATE：上次计划时间 + 周期(ms)，不漂移
	next, err := NextByType("FIXED_RATE", "1500", base, base.Add(200*time.Millisecond))
	if err != nil {
		t.Fatalf("fixed_rate: %v", err)
	}
	if want := base.Add(1500 * time.Millisecond); !next.Equal(want) {
		t.Fatalf("fixed_rate next=%s want=%s", next, want)
	}

	// FIXED_DELAY：以实际触发时刻为基准 + 周期(ms)
	firedAt := base.Add(700 * time.Millisecond)
	next, err = NextByType("FIXED_DELAY", "1500", base, firedAt)
	if err != nil {
		t.Fatalf("fixed_delay: %v", err)
	}
	if want := firedAt.Add(1500 * time.Millisecond); !next.Equal(want) {
		t.Fatalf("fixed_delay next=%s want=%s", next, want)
	}

	// API 不自动触发
	if n, _ := NextByType("API", "", base, base); n.Year() != 9999 {
		t.Fatalf("api should never auto-fire, got %s", n)
	}
	// 依赖触发同理：没有自己的调度时间，只被上游实例的成功回调拉起
	if n, err := NextByType("DEPENDENCY", "", base, base); err != nil || n.Year() != 9999 {
		t.Fatalf("dependency should never auto-fire, got %s err=%v", n, err)
	}
	if ts, err := Preview("DEPENDENCY", "", base, 5); err != nil || len(ts) != 0 {
		t.Fatalf("dependency preview = %v err=%v, want 空", ts, err)
	}
}

func TestFrequencyIntervalUpperBound(t *testing.T) {
	now := time.Now()
	// 与兼容协议的 FREQUENT_JOB_MAX_INTERVAL 默认值同值：120000ms
	if _, err := NextByType("FIXED_RATE", "120001", now, now); err == nil {
		t.Fatal("interval over 120000ms must be rejected")
	}
	if _, err := NextByType("FIXED_RATE", "0", now, now); err == nil {
		t.Fatal("non-positive interval must be rejected")
	}
	if _, err := NextByType("FIXED_DELAY", "abc", now, now); err == nil {
		t.Fatal("non-numeric interval must be rejected")
	}
}

func TestPreviewSubSecondResolution(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	times, err := Preview("FIXED_RATE", "100", now, 3)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(times) != 3 {
		t.Fatalf("want 3 samples, got %d", len(times))
	}
	for i, got := range times {
		want := now.Add(time.Duration(i+1) * 100 * time.Millisecond)
		if !got.Equal(want) {
			t.Fatalf("sample %d: got %s want %s", i+1, got, want)
		}
	}
}

// 时间轮的语义保证是"毫秒级、不早于目标时刻"。用一段连续触发取**中位数**判定精度：
// 单样本断言在 -race/GC 抖动下会误报（曾因一次 240ms 的调度停顿把这个用例打挂），
// 而中位数既能守住 10ms tick 的精度证据，又对偶发抖动免疫。
func TestWheelFiresWithMillisecondPrecision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWheel(wheelTickSize, 64)
	w.Start(ctx)

	const (
		count    = 20
		interval = 25 * time.Millisecond
	)
	start := time.Now()
	fireCh := make(chan time.Duration, count)
	for i := 1; i <= count; i++ {
		target := start.Add(time.Duration(i) * interval)
		w.Add(uint64(i), target, func() { fireCh <- time.Since(start) })
	}

	var lateness []time.Duration
	for i := 0; i < count; i++ {
		select {
		case got := <-fireCh:
			// 允许一个 tick 的量化提前，绝不允许提前一个 tick 以上
			if got < time.Duration(i+1)*interval-wheelTickSize {
				t.Fatalf("第 %d 次触发过早: %v", i+1, got)
			}
			lateness = append(lateness, got-time.Duration(i+1)*interval)
		case <-time.After(5 * time.Second):
			t.Fatalf("wheel 只触发了 %d/%d 次", i, count)
		}
	}
	sort.Slice(lateness, func(a, b int) bool { return lateness[a] < lateness[b] })
	median := lateness[len(lateness)/2]
	if median > 3*wheelTickSize {
		t.Fatalf("中位延迟 %v 超过 3 个 tick (%v)，毫秒精度退化", median, 3*wheelTickSize)
	}
	if lateness[len(lateness)-1] > 500*time.Millisecond {
		t.Fatalf("最差一次延迟 %v 过大", lateness[len(lateness)-1])
	}
}

// 回归：驱动协程停顿（GC/调度饥饿）后进入追赶状态，此时 cur 对应的逻辑时刻比真实时间早，
// 新挂的"100ms 后"必须按逻辑时钟排到 1.6s 之后，不能被追赶循环当场踩掉。
// 老实现按 time.Until(真实 now) 换算 → 只算 1 格 → 停顿期间被连发十几次，之后又长时间静默。
func TestAddUsesLogicalClockWhileDriverCatchesUp(t *testing.T) {
	const tick = 10 * time.Millisecond
	w := NewWheel(tick, 64)
	w.mu.Lock()
	w.clock = time.Now().Add(-1500 * time.Millisecond) // 模拟落后的逻辑时钟
	w.mu.Unlock()

	fireCh := make(chan struct{}, 8)
	w.Add(1, time.Now().Add(100*time.Millisecond), func() { fireCh <- struct{}{} })

	for i := 0; i < 150; i++ { // 追赶 1.5s
		w.advance()
	}
	select {
	case <-fireCh:
		t.Fatal("追赶期间就把任务踩发了，应按逻辑时钟排到 1.6s 后")
	default:
	}
	for i := 0; i < 20; i++ {
		w.advance()
	}
	select {
	case <-fireCh:
	case <-time.After(time.Second):
		t.Fatal("补够 170 格后仍未触发")
	}
}

func TestWheelReplaceAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWheel(wheelTickSize, 64)
	w.Start(ctx)

	fired := make(chan struct{}, 4)
	now := time.Now()
	w.Add(7, now.Add(50*time.Millisecond), func() { fired <- struct{}{} })
	w.Add(7, now.Add(50*time.Millisecond), func() { fired <- struct{}{} }) // 同 id 覆盖，只应触发一次
	w.Add(8, now.Add(50*time.Millisecond), func() { fired <- struct{}{} })
	w.Cancel(8) // 撤销，不应触发

	time.Sleep(400 * time.Millisecond)
	if n := len(fired); n != 1 {
		t.Fatalf("want exactly 1 fire (replace dedup + cancel), got %d", n)
	}
	if p := w.Pending(); p != 0 {
		t.Fatalf("wheel should drain fired/cancelled handlers, pending=%d", p)
	}
}

func TestWheelSpansMultipleRevolutions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// slotNum=4, tick=10ms -> 一圈 40ms；250ms 需要约 6 圈，验证 rounds 降级
	w := NewWheel(10*time.Millisecond, 4)
	w.Start(ctx)

	done := make(chan struct{})
	w.Add(1, time.Now().Add(250*time.Millisecond), func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("multi-round task never fired")
	}
}

func TestDailyTimeIntervalWindowSemantics(t *testing.T) {
	// 2026-09-23 是周三（chineseWeekday=3）
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, time.Local)
	const expr = `{"interval":30,"intervalUnit":"MINUTES","startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00"}`

	// 窗口内：基准+间隔直接命中
	got, err := NextByType("DAILY_TIME_INTERVAL", expr, noon, noon)
	if err != nil {
		t.Fatalf("in-window: %v", err)
	}
	if want := noon.Add(30 * time.Minute); !got.Equal(want) {
		t.Fatalf("in-window got %s want %s", got, want)
	}

	// 早于窗口起点：吸附到当天 09:00:00
	early := time.Date(2026, 9, 23, 7, 0, 0, 0, time.Local)
	got, _ = NextByType("DAILY_TIME_INTERVAL", expr, early, early)
	// 07:00+30min=07:30 仍早于窗口起点 → 吸附到 09:00:00（兼容协议 calculateInRangeTime 的行为）
	if want := time.Date(2026, 9, 23, 9, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Fatalf("before-window got %s want %s", got, want)
	}

	// 越过窗口终点：顺延到次日 09:00（17:45+30min=18:15 > 18:00）
	late := time.Date(2026, 9, 23, 17, 45, 0, 0, time.Local)
	got, _ = NextByType("DAILY_TIME_INTERVAL", expr, late, late)
	if want := time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Fatalf("past-window got %s want %s", got, want)
	}
}

func TestDailyTimeIntervalDaysOfWeekSkip(t *testing.T) {
	// 只在周六(6)、周日(7)跑；2026-09-23 是周三
	const expr = `{"interval":10,"intervalUnit":"MINUTES","startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00","daysOfWeek":[6,7]}`
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	got, err := NextByType("DAILY_TIME_INTERVAL", expr, base, base)
	if err != nil {
		t.Fatalf("daysOfWeek: %v", err)
	}
	// 应跳到本周六 09-26 的窗口起点
	if want := time.Date(2026, 9, 26, 9, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Fatalf("daysOfWeek got %s want %s", got, want)
	}
	if w := chineseWeekday(got); w != 6 {
		t.Fatalf("expected Saturday(6), got %d", w)
	}
}

func TestDailyTimeIntervalValidation(t *testing.T) {
	now := time.Now()
	bad := map[string]string{
		"非 JSON":        "not-json",
		"缺 interval":    `{"startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00"}`,
		"间隔非正":          `{"interval":0,"startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00"}`,
		"终点早于起点":        `{"interval":10,"startTimeOfDay":"18:00:00","endTimeOfDay":"09:00:00"}`,
		"时间格式非法":        `{"interval":10,"startTimeOfDay":"25:00:00","endTimeOfDay":"26:00:00"}`,
		"单位非法":          `{"interval":10,"intervalUnit":"DAYS","startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00"}`,
		"daysOfWeek 越界": `{"interval":10,"startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00","daysOfWeek":[9]}`,
	}
	for name, expr := range bad {
		if _, err := NextByType("DAILY_TIME_INTERVAL", expr, now, now); err == nil {
			t.Fatalf("%s: 应当校验失败但通过了", name)
		}
	}
	// HH:mm 两段也应被接受（对齐 Quartz TimeOfDay.from）
	ok := `{"interval":10,"startTimeOfDay":"09:00","endTimeOfDay":"18:00"}`
	if _, err := NextByType("DAILY_TIME_INTERVAL", ok, now, now); err != nil {
		t.Fatalf("HH:mm 应被接受: %v", err)
	}
}

func TestDailyTimeIntervalPreviewMonotonic(t *testing.T) {
	const expr = `{"interval":20,"intervalUnit":"MINUTES","startTimeOfDay":"09:00:00","endTimeOfDay":"09:40:00"}`
	from := time.Date(2026, 9, 23, 9, 0, 0, 0, time.Local)
	times, err := Preview("DAILY_TIME_INTERVAL", expr, from, 6)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	// 09:20 09:40 | 次日 09:00 09:20 09:40 | 第三日 09:00
	want := []time.Time{
		time.Date(2026, 9, 23, 9, 20, 0, 0, time.Local),
		time.Date(2026, 9, 23, 9, 40, 0, 0, time.Local),
		time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local),
		time.Date(2026, 9, 24, 9, 20, 0, 0, time.Local),
		time.Date(2026, 9, 24, 9, 40, 0, 0, time.Local),
		time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local),
	}
	for i := range want {
		if !times[i].Equal(want[i]) {
			t.Fatalf("sample %d got %s want %s", i, times[i], want[i])
		}
	}
}

// 回归：服务停摆数小时后，next_trigger_time 已远在过去。DAILY_TIME_INTERVAL 若照
// base+interval 继续算，结果仍落在过去，onFire 会在每个 tick 立即补一次——10s 间隔停
// 3 小时就是单任务上千个实例，以写库速度连发。与 FIXED_RATE 同口径：从 now 起算，不追补。
func TestDailyTimeIntervalStaleBaseDoesNotBurst(t *testing.T) {
	now := time.Date(2026, 9, 23, 16, 0, 0, 0, time.Local)
	stale := now.Add(-3 * time.Hour)

	const expr = `{"interval":10,"intervalUnit":"SECONDS","startTimeOfDay":"00:00:00","endTimeOfDay":"23:59:59"}`
	got, err := NextByType("DAILY_TIME_INTERVAL", expr, stale, now)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if !got.After(now) {
		t.Fatalf("stale base next=%s 仍在 now(%s) 之前，onFire 会立即重挂造成连续追补", got, now)
	}
	if want := now.Add(10 * time.Second); !got.Equal(want) {
		t.Fatalf("stale got %s want %s", got, want)
	}

	// 停机后当前时刻在窗口外：只顺延到下一个窗口，不补今天已错过的历史
	const windowed = `{"interval":10,"intervalUnit":"SECONDS","startTimeOfDay":"09:00:00","endTimeOfDay":"10:00:00"}`
	got, err = NextByType("DAILY_TIME_INTERVAL", windowed, stale, now)
	if err != nil {
		t.Fatalf("stale windowed: %v", err)
	}
	if want := time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Fatalf("stale windowed got %s want %s", got, want)
	}
}
