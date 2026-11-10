package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v2 "encoding/json/v2"
)

// 每日固定间隔（DAILY_TIME_INTERVAL），表达式与语义和兼容协议的
// DailyTimeIntervalStrategyHandler.DailyTimeIntervalExpress 一致：
//
//	{"interval":10,"intervalUnit":"SECONDS","startTimeOfDay":"08:00:00","endTimeOfDay":"22:00:00","daysOfWeek":[1,2,3,4,5]}
//
// 含义：在 daysOfWeek 指定的那些天里（空=每天），只在 [startTimeOfDay, endTimeOfDay]
// 这个每日时间窗内、每 interval 触发一次；窗口外则顺延到下一个有效日的窗口起点。
type dailyIntervalExpress struct {
	Interval       *int64 `json:"interval"`
	StartTimeOfDay string `json:"startTimeOfDay"`
	EndTimeOfDay   string `json:"endTimeOfDay"`
	IntervalUnit   string `json:"intervalUnit"`
	DaysOfWeek     []int  `json:"daysOfWeek"`
}

type timeOfDay struct{ h, m, s int }

// maxDailyProbeDays 有效日/窗口推进的最大探测次数，防御 daysOfWeek 全非法导致死循环。
const maxDailyProbeDays = 800

// parseDailyInterval 校验并解析每日固定间隔表达式。
func parseDailyInterval(expr string) (intervalMs int64, start, end timeOfDay, days map[int]bool, err error) {
	var ep dailyIntervalExpress
	if e := v2.Unmarshal([]byte(strings.TrimSpace(expr)), &ep); e != nil {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval 表达式需为 JSON 对象，got %q", expr)
	}
	if ep.Interval == nil || *ep.Interval <= 0 {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval.interval 需为正整数，got %v", ep.Interval)
	}
	start, err = parseTimeOfDay(ep.StartTimeOfDay)
	if err != nil {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval.startTimeOfDay: %w", err)
	}
	end, err = parseTimeOfDay(ep.EndTimeOfDay)
	if err != nil {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval.endTimeOfDay: %w", err)
	}
	if end.before(start) {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval 终点(%s)不能早于起点(%s)", ep.EndTimeOfDay, ep.StartTimeOfDay)
	}

	// 单位缺省为秒，与兼容协议 Optional.ofNullable(intervalUnit).orElse(SECONDS) 的行为一致
	unit := strings.ToUpper(strings.TrimSpace(ep.IntervalUnit))
	if unit == "" {
		unit = "SECONDS"
	}
	mult, ok := map[string]int64{
		"MILLISECONDS": 1,
		"SECONDS":      time.Second.Milliseconds(),
		"MINUTES":      time.Minute.Milliseconds(),
		"HOURS":        time.Hour.Milliseconds(),
	}[unit]
	if !ok {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval.intervalUnit 不支持 %q（可用 MILLISECONDS/SECONDS/MINUTES/HOURS）", ep.IntervalUnit)
	}
	intervalMs = *ep.Interval * mult
	if intervalMs <= 0 {
		return 0, start, end, nil, fmt.Errorf("daily_time_interval 换算后间隔必须 > 0ms")
	}

	days = make(map[int]bool, len(ep.DaysOfWeek))
	for _, d := range ep.DaysOfWeek {
		if d < 1 || d > 7 {
			return 0, start, end, nil, fmt.Errorf("daily_time_interval.daysOfWeek 取值需在 1(周一)~7(周日)，got %d", d)
		}
		days[d] = true
	}
	return intervalMs, start, end, days, nil
}

// parseTimeOfDay 接受 "HH:mm:ss" 或 "HH:mm"，与 Quartz/兼容协议的 TimeOfDay.from 解析一致。
func parseTimeOfDay(s string) (timeOfDay, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return timeOfDay{}, fmt.Errorf("时间格式需为 HH:mm:ss 或 HH:mm，got %q", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return timeOfDay{}, fmt.Errorf("时间格式非法 %q: %w", s, err)
		}
		nums[i] = n
	}
	tod := timeOfDay{h: nums[0], m: nums[1], s: nums[2]}
	if len(parts) == 2 {
		tod.s = 0
	}
	if tod.h < 0 || tod.h > 23 || tod.m < 0 || tod.m > 59 || tod.s < 0 || tod.s > 59 {
		return timeOfDay{}, fmt.Errorf("时间超出范围 %q", s)
	}
	return tod, nil
}

func (t timeOfDay) before(o timeOfDay) bool { return t.h*3600+t.m*60+t.s < o.h*3600+o.m*60+o.s }

// chineseWeekday 国内习惯：周一=1 … 周日=7（与兼容协议 TimeUtils.calculateWeek 一致）。
func chineseWeekday(t time.Time) int {
	if w := int(t.Weekday()); w != 0 {
		return w
	}
	return 7
}

// atTimeOfDay 把 t 的日期部分保留、时刻改写为 tod（毫秒清零，同兼容协议的 simpleSetCalendar）。
func atTimeOfDay(t time.Time, tod timeOfDay) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), tod.h, tod.m, tod.s, 0, t.Location())
}

func startOfNextDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}

// inDailyRange 移植兼容协议的 calculateInRangeTime：返回 >=time 且落在有效日窗口内的最近时刻。
func inDailyRange(t time.Time, start, end timeOfDay, days map[int]bool) (time.Time, error) {
	for i := 0; i < maxDailyProbeDays; i++ {
		// 非激活日：跳到次日 00:00 重新判定
		if len(days) > 0 && !days[chineseWeekday(t)] {
			t = startOfNextDay(t)
			continue
		}
		todayStart := atTimeOfDay(t, start)
		if t.Before(todayStart) {
			return todayStart, nil // 窗口未开始 → 取窗口起点
		}
		todayEnd := atTimeOfDay(t, end)
		if !t.After(todayEnd) {
			return t, nil // 落在窗口内
		}
		t = startOfNextDay(t) // 今日窗口已过 → 顺延次日
	}
	return time.Time{}, fmt.Errorf("每日固定间隔：连续 %d 天都无匹配的激活日，请检查 daysOfWeek", maxDailyProbeDays)
}

// nextDailyTimeInterval 每日固定间隔的下一次触发：基准 + 间隔后，吸附到最近的窗口内时刻。
// 基准早于 now（例如服务停摆数小时）时从 now 起算，与 FIXED_RATE 同口径不追补历史：
// 否则 base+interval 仍落在过去，时间轮会把它当成"立即到期"，每个 tick 补一个历史时刻，
// 10s 间隔停 3 小时就是单任务上千个实例。
func nextDailyTimeInterval(expr string, base, now time.Time) (time.Time, error) {
	intervalMs, start, end, days, err := parseDailyInterval(expr)
	if err != nil {
		return time.Time{}, err
	}
	if base.Before(now) {
		base = now
	}
	return inDailyRange(base.Add(time.Duration(intervalMs)*time.Millisecond), start, end, days)
}

// validateDailyInterval 供管理面在保存前校验表达式。
func validateDailyInterval(expr string) error {
	_, _, _, _, err := parseDailyInterval(expr)
	return err
}
