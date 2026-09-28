// 多任务执行类型的演示处理器：BROADCAST / MAP / MAP_REDUCE。
//
// 它们同时也是 scripts/verify-map-broadcast.sh 的断言靶子，所以每个处理器都把自己的
// 关键决定写进 SetResult 文本里（派生了几个、哪一片故意失败、汇总出了什么），
// 验收脚本只要读实例与子任务的结果就能判断链路对不对。
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gojob/worker"
)

// nodeID 本机标识：广播要证明"每个在线节点各收到一份"，结果里必须带上是谁跑的。
func nodeID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// params 取本次执行的参数：实例参数优先（与兼容协议的 CommonUtils.parseParams 一致）。
func params(ctx *worker.TaskContext) string {
	if strings.TrimSpace(ctx.InstanceParams) != "" {
		return strings.TrimSpace(ctx.InstanceParams)
	}
	return strings.TrimSpace(ctx.JobParams)
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// broadcastEcho 广播任务：每台在线节点各执行一份，结果里带上本机标识。
// 选 BROADCAST 时这个处理器不需要写任何 map 代码——派生是 server 在建单时做的。
type broadcastEcho struct{}

func (broadcastEcho) Process(ctx *worker.TaskContext) error {
	who := nodeID()
	ctx.Infof("广播分片落在节点 %s，任务号=%s", who, ctx.TaskKey)
	ctx.SetResult(fmt.Sprintf("node=%s key=%s", who, ctx.TaskKey))
	return nil
}

// mapSplit 最简单的 Map：根任务把 "a,b,c" 拆成子任务，每个子任务转大写后回报。
type mapSplit struct{}

func (mapSplit) Process(ctx *worker.TaskContext) error {
	if ctx.IsRootTask() {
		items := strings.Split(params(ctx), ",")
		list := make([]any, 0, len(items))
		for _, it := range items {
			if it = strings.TrimSpace(it); it != "" {
				list = append(list, map[string]string{"word": it})
			}
		}
		if len(list) == 0 {
			return fmt.Errorf("mapSplit 需要一个逗号分隔的参数作为分片内容")
		}
		keys, err := ctx.Map(list, "UPPER")
		if err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("已派生 %d 片：%s", len(keys), strings.Join(keys, " ")))
		return nil
	}
	var payload struct {
		Word string `json:"word"`
	}
	if err := ctx.SubTask(&payload); err != nil {
		return err
	}
	ctx.SetResult(strings.ToUpper(payload.Word))
	return nil
}

// mapFailOne 让指定那一片故意失败，用来验证"任一子任务失败 ⇒ 实例失败"。
// 参数格式：分片内容,坏片序号（从 1 开始）。
type mapFailOne struct{}

func (mapFailOne) Process(ctx *worker.TaskContext) error {
	raw := params(ctx)
	parts := strings.Split(raw, ",")
	if ctx.IsRootTask() {
		if len(parts) < 2 {
			return fmt.Errorf("mapFailOne 参数应为 词1,词2,...,坏片序号")
		}
		list := make([]any, 0, len(parts)-1)
		for _, w := range parts[:len(parts)-1] {
			if w = strings.TrimSpace(w); w != "" {
				list = append(list, map[string]any{"word": w, "bad": len(list)+1 == atoiOr(parts[len(parts)-1], -1)})
			}
		}
		if _, err := ctx.Map(list, "MAY_FAIL"); err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("已派生 %d 片，第 %s 片会失败", len(list), parts[len(parts)-1]))
		return nil
	}
	var payload struct {
		Word string `json:"word"`
		Bad  bool   `json:"bad"`
	}
	if err := ctx.SubTask(&payload); err != nil {
		return err
	}
	if payload.Bad {
		return fmt.Errorf("故意失败：word=%s", payload.Word)
	}
	ctx.SetResult("ok:" + payload.Word)
	return nil
}

// deepMap 多级 Map：根任务拆成几组，每组再拆成几项。兼容协议允许子任务继续 map，这里照做。
// 参数格式：组数,每组项数。
type deepMap struct{}

func (deepMap) Process(ctx *worker.TaskContext) error {
	raw := params(ctx)
	parts := strings.Split(raw, ",")
	if ctx.IsRootTask() {
		groups := atoiOr(parts[0], 2)
		list := make([]any, 0, groups)
		for i := 1; i <= groups; i++ {
			list = append(list, map[string]any{"group": i, "items": atoiOr(parts[1], 2)})
		}
		if _, err := ctx.Map(list, "GROUP"); err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("根任务派生 %d 组", groups))
		return nil
	}
	var group struct {
		Group int `json:"group"`
		Item  int `json:"item"`
		Items int `json:"items"`
	}
	if err := ctx.SubTask(&group); err != nil {
		return err
	}
	if group.Items > 0 { // 第二层：组内再拆项（第三层不再拆，避免无限递归）
		list := make([]any, 0, group.Items)
		for i := 1; i <= group.Items; i++ {
			list = append(list, map[string]any{"group": group.Group, "item": i, "items": 0})
		}
		if _, err := ctx.Map(list, "ITEM"); err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("组 %d 派生 %d 项", group.Group, group.Items))
		return nil
	}
	ctx.SetResult(fmt.Sprintf("g%d-i%d", group.Group, group.Item))
	return nil
}

// mrSum 的 MapReduce 形态：根任务派生若干数字片，每片回报一个数，reduce 求和。
// 参数格式：数字列表，如 "1,2,3,4"；末尾可加 "bad=N" 让第 N 片失败，验证 reduce 的容错。
type mrSum struct{}

func (mrSum) Process(ctx *worker.TaskContext) error {
	raw := params(ctx)
	if ctx.IsRootTask() {
		nums, badAt := parseMrParams(raw)
		if len(nums) == 0 {
			return fmt.Errorf("mrSum 需要逗号分隔的数字作为参数")
		}
		list := make([]any, 0, len(nums))
		for i, n := range nums {
			list = append(list, map[string]any{"n": n, "bad": badAt > 0 && i+1 == badAt})
		}
		if _, err := ctx.Map(list, "NUM"); err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("已派生 %d 个数字片", len(nums)))
		return nil
	}
	var payload struct {
		N   int  `json:"n"`
		Bad bool `json:"bad"`
	}
	if err := ctx.SubTask(&payload); err != nil {
		return err
	}
	if payload.Bad {
		return fmt.Errorf("故意失败：n=%d", payload.N)
	}
	ctx.SetResult(strconv.Itoa(payload.N))
	return nil
}

// Reduce 的返回值就是本次执行的最终结论：这里对部分失败采取"容忍并如实报告"的策略，
// 只有全部子任务都失败才判整体失败——这正是 MAP_REDUCE 相对 MAP 的能力差别。
func (mrSum) Reduce(ctx *worker.TaskContext, results []worker.TaskResult) error {
	sum, ok, failed := 0, 0, 0
	var detail []string
	for _, r := range results {
		if !r.Success {
			failed++
			detail = append(detail, "失败:"+r.Result)
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(r.Result))
		if err != nil {
			failed++
			detail = append(detail, "无法解析:"+r.Result)
			continue
		}
		ok++
		sum += n
	}
	ctx.SetResult(fmt.Sprintf("收到 %d 片，成功 %d，失败 %d，总和=%d", len(results), ok, failed, sum))
	if ok == 0 {
		return fmt.Errorf("全部 %d 个子任务都失败了：%s", failed, strings.Join(detail, "; "))
	}
	if failed > 0 {
		ctx.Warnf("有 %d 个子任务失败，reduce 已容忍：%s", failed, strings.Join(detail, "; "))
	}
	return nil
}

func parseMrParams(raw string) (nums []int, badAt int) {
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if v, ok := strings.CutPrefix(p, "bad="); ok {
			badAt = atoiOr(v, 0)
			continue
		}
		nums = append(nums, atoiOr(p, 0))
	}
	return nums, badAt
}

// sleepMap 慢子任务：根任务派生 count 片，每片睡 seconds 秒。
// 停实例那条链路需要"取消指令还在路上、树远没收敛"的现场，秒级完成的子任务演不出来。
// 参数格式：片数,秒数。
type sleepMap struct{}

func (sleepMap) Process(ctx *worker.TaskContext) error {
	count, seconds := parseTwo(params(ctx))
	if ctx.IsRootTask() {
		list := make([]any, 0, count)
		for i := 1; i <= count; i++ {
			list = append(list, map[string]any{"idx": i, "seconds": seconds})
		}
		if _, err := ctx.Map(list, "SLEEP"); err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("已派生 %d 片，每片睡 %ds", count, seconds))
		return nil
	}
	var payload struct {
		Idx     int `json:"idx"`
		Seconds int `json:"seconds"`
	}
	if err := ctx.SubTask(&payload); err != nil {
		return err
	}
	select {
	case <-time.After(time.Duration(payload.Seconds) * time.Second):
		ctx.SetResult(fmt.Sprintf("睡满 %ds：%d", payload.Seconds, payload.Idx))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reduce 让 sleepMap 也能配成 MAP_REDUCE：验收脚本要用它证明"被停止的实例不会补 reduce"。
func (sleepMap) Reduce(ctx *worker.TaskContext, results []worker.TaskResult) error {
	ok := 0
	for _, r := range results {
		if r.Success {
			ok++
		}
	}
	ctx.SetResult(fmt.Sprintf("reduce 收到 %d 片，成功 %d", len(results), ok))
	return nil
}

// retryMap 根任务派生子任务后**故意在第一次尝试时失败**，让调度立刻重派父任务。
// 它同时验证两件事：旧一代里还没收敛的子任务被作废（DISCARDED，不重复执行副作用），
// 以及父任务重跑后能重新 map 出一批新任务（派生幂等键里带了尝试次数，否则会一条都不建）。
//
// "第几次尝试"读 ctx.Attempts 而不是本机内存里的记账：重派很可能换一台机器，
// 进程内的状态在换机之后就是错的。
// 参数格式：片数,秒数。
type retryMap struct{}

func (retryMap) Process(ctx *worker.TaskContext) error {
	count, seconds := parseTwo(params(ctx))
	if ctx.IsRootTask() {
		list := make([]any, 0, count)
		for i := 1; i <= count; i++ {
			list = append(list, map[string]any{"idx": i, "seconds": seconds})
		}
		if _, err := ctx.Map(list, "RETRY"); err != nil {
			return err
		}
		ctx.SetResult(fmt.Sprintf("第 %d 次尝试派生 %d 片", ctx.Attempts, count))
		if ctx.Attempts == 1 {
			return fmt.Errorf("根任务第一次尝试按设计失败，触发重派")
		}
		return nil
	}
	var payload struct {
		Idx     int `json:"idx"`
		Seconds int `json:"seconds"`
	}
	if err := ctx.SubTask(&payload); err != nil {
		return err
	}
	select {
	case <-time.After(time.Duration(payload.Seconds) * time.Second):
		ctx.SetResult(fmt.Sprintf("片 %d 完成", payload.Idx))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func parseTwo(raw string) (count, seconds int) {
	count, seconds = 3, 5
	parts := strings.Split(raw, ",")
	if len(parts) > 0 {
		count = atoiOr(parts[0], count)
	}
	if len(parts) > 1 {
		seconds = atoiOr(parts[1], seconds)
	}
	return count, seconds
}

// registerMapProcessors 把上面这些挂进 demo worker。
func registerMapProcessors() {
	worker.Register("broadcastEcho", broadcastEcho{})
	worker.Register("mapSplit", mapSplit{})
	worker.Register("mapFailOne", mapFailOne{})
	worker.Register("deepMap", deepMap{})
	worker.Register("mrSum", mrSum{})
	worker.Register("sleepMap", sleepMap{})
	worker.Register("retryMap", retryMap{})
}
