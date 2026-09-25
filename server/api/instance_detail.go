// 实例详情与子任务查询：对齐 PowerJob 的 POST /instance/detailPlus。
//
// PowerJob 这一步是 server 反向 ask TaskTracker 所在 worker、由它查本机 task_info 表；
// gojob 的任务树就在库里的 instance_tasks，所以同一份数据不必现场问执行节点，
// 实例结束、worker 下线之后依然可查（这是把子任务落库换来的能力）。
//
// UI 上那个 "select * from task_info where ___ limit 10" 输入框照搬，但后端**不拼 SQL**：
// 把用户文本原样交给数据库等于给控制台开注入口（PowerJob 只有关键字黑名单，而且它是在
// 自己 worker 的本地库上执行，越权面比在我们的共享库上小得多）。
// 这里换成只认白名单语法片的解析器，认不出来就明确说支持什么。
package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// taskInfoLimit 与 PowerJob 那句写死的后缀 limit 10 同值。
const taskInfoLimit = 10

// TaskInfoRow 子任务表的一行，列按 PowerJob 控制台 task_info 的展示口径给。
type TaskInfoRow struct {
	TaskID        string    `json:"task_id"`        // 层次 ID："0" / "0.3" / reduce 是 "9999"
	TaskName      string    `json:"task_name"`      // map() 的第二参；内部任务是 OMS_* 保留名
	TaskContent   string    `json:"task_content"`   // 子任务负载（JSON）
	ProcessorAddr string    `json:"processor_addr"` // 认领/执行该任务的节点
	FailedCnt     int       `json:"failed_cnt"`     // 已失败次数 = 尝试次数 - 1
	Status        string    `json:"status"`
	StatusStr     string    `json:"status_str"`
	CreatedTime   time.Time `json:"created_time"`
	ModifiedTime  time.Time `json:"last_modified_time"`
	Result        string    `json:"result"`
}

// TaskStats 任务统计，PowerJob 详情弹窗里那行 "Task 统计:" 的内容。
type TaskStats struct {
	Total     int `json:"total"`
	Waiting   int `json:"waiting"`
	Running   int `json:"running"`
	Succeed   int `json:"succeed"`
	Failed    int `json:"failed"`
	Stopped   int `json:"stopped"`
	Discarded int `json:"discarded"` // gojob 独有：父任务重派时作废的后代
}

// InstanceDetailResp 实例详情弹窗的全部数据。
type InstanceDetailResp struct {
	Instance   store.JobInstance `json:"instance"`
	TaskStats  TaskStats         `json:"task_stats"`
	Tasks      []TaskInfoRow     `json:"tasks"`
	Total      int               `json:"total"` // 命中过滤条件的总行数，不是本页行数
	QueryError string            `json:"query_error"`
}

var taskStatusCN = map[string]string{
	protocol.TaskWaiting:   "等待派发",
	protocol.TaskRunning:   "运行中",
	protocol.TaskSucceed:   "成功",
	protocol.TaskFailed:    "失败",
	protocol.TaskStopped:   "已停止",
	protocol.TaskDiscarded: "已作废",
}

// 白名单语法：列名与写法全部写死，用户文本只作为"值"进占位符，永远不进 SQL 文本。
var (
	qStatusEq = regexp.MustCompile(`^status\s*=\s*(\d+)$`)
	qStatusIn = regexp.MustCompile(`^status\s+in\s*\(([^)]*)\)$`)
	qNameEq   = regexp.MustCompile(`^task_name\s*=\s*'([^']*)'$`)
	qNameIn   = regexp.MustCompile(`^task_name\s+in\s*\(([^)]*)\)$`)
	qParentEq = regexp.MustCompile(`^parent_task_id\s*=\s*(\d+)$`)
	qOrder    = regexp.MustCompile(`(?i)^order\s+by\s+(id|created_time|last_modified_time|status|task_name)(\s+(asc|desc))?$`)
	qLimit    = regexp.MustCompile(`(?i)^limit\s+\d+$`)
)

const queryHelp = "支持的写法：status = N / status in (N, N) / task_name = 'X' / task_name in ('X', 'Y') / " +
	"parent_task_id = N，条件之间用 and 连接；可选 order by id|created_time|last_modified_time|status|task_name [asc|desc]。" +
	"status 的数字按 PowerJob 的 TaskStatus 序号解释（5=失败、6=成功），也可以直接写状态名。"

// orderColumn 把查询里出现的排序列名翻成真实列名。
// 对外名字必须与响应 JSON 的字段名一致（created_time / last_modified_time），否则控制台
// 照搬 PowerJob 的那句默认查询 "status in (5, 6) order by last_modified_time desc" 一开弹窗
// 就报 no such column；库里这两列叫 create_time / modify_time，所以在这一层映射掉。
// 映射表取值写死，用户文本永远不进 SQL 文本。
var orderColumn = map[string]string{
	"id":                 "id",
	"status":             "status",
	"task_name":          "task_name",
	"created_time":       "create_time",
	"last_modified_time": "modify_time",
}

// parseTaskQuery 把 customQuery 翻成 (条件, 参数, 排序)。
func parseTaskQuery(qs string) (where []string, args []any, order string, err error) {
	order = "id asc"
	qs = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(qs), ";"))
	if qs == "" {
		return nil, nil, order, nil
	}
	// 先摘掉 order by / limit 尾巴，剩下的才是条件
	if i := indexFold(qs, "order by"); i >= 0 {
		tail := strings.TrimSpace(qs[i:])
		qs = strings.TrimSpace(qs[:i])
		m := qOrder.FindStringSubmatch(tail)
		if m == nil {
			return nil, nil, "", fmt.Errorf("无法识别的排序写法：%q。%s", tail, queryHelp)
		}
		dir := "asc"
		if m[3] != "" {
			dir = strings.ToLower(m[3])
		}
		order = orderColumn[m[1]] + " " + dir
	}
	if i := indexFold(qs, "limit"); i >= 0 {
		tail := strings.TrimSpace(qs[i:])
		if !qLimit.MatchString(tail) {
			return nil, nil, "", fmt.Errorf("无法识别的限量写法：%q。%s", tail, queryHelp)
		}
		qs = strings.TrimSpace(qs[:i]) // 条数由服务端固定，忽略用户写的值（PowerJob 也是写死）
	}
	qs = strings.TrimSpace(strings.TrimSuffix(qs, ";"))
	// 只把开头的 where 关键字去掉，**不能整体 ToLower**：task_name 是大小写敏感的，
	// 用户写 'SPLIT' 就该查 'SPLIT'。
	if len(qs) >= 6 && strings.EqualFold(qs[:5], "where") && (qs[5] == ' ' || qs[5] == '\t') {
		qs = strings.TrimSpace(qs[5:])
	}
	if qs == "" {
		return nil, nil, order, nil
	}
	for _, part := range strings.Split(qs, " and ") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		switch {
		case qStatusEq.MatchString(p):
			n := qStatusEq.FindStringSubmatch(p)[1]
			where, args = append(where, "status = ?"), append(args, taskStatusFromOrdinal(n))
		case qStatusIn.MatchString(p):
			list, e := intList(qStatusIn.FindStringSubmatch(p)[1])
			if e != nil {
				return nil, nil, "", e
			}
			statuses := make([]any, 0, len(list))
			for _, n := range list {
				statuses = append(statuses, taskStatusFromOrdinal(strconv.Itoa(n)))
			}
			where, args = append(where, "status IN ?"), append(args, statuses)
		case qNameEq.MatchString(p):
			where, args = append(where, "task_name = ?"), append(args, qNameEq.FindStringSubmatch(p)[1])
		case qNameIn.MatchString(p):
			names, e := strList(qNameIn.FindStringSubmatch(p)[1])
			if e != nil {
				return nil, nil, "", e
			}
			where, args = append(where, "task_name IN ?"), append(args, names)
		case qParentEq.MatchString(p):
			n := qParentEq.FindStringSubmatch(p)[1]
			where, args = append(where, "parent_task_id = ?"), append(args, n)
		default:
			return nil, nil, "", fmt.Errorf("无法识别的查询条件：%q。%s", p, queryHelp)
		}
	}
	return where, args, order, nil
}

// taskStatusFromOrdinal 认 PowerJob 的 TaskStatus 序号，也认 gojob 自己的状态名：
// 从 PowerJob 习惯里敲 "status in (5,6)" 的人不少，而我们的库里存的是字符串。
func taskStatusFromOrdinal(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return strings.ToUpper(raw)
	}
	if s, ok := powerJobTaskOrdinal[n]; ok {
		return s
	}
	return strconv.Itoa(n) // 认不出来就原样比较：命中 0 行，比报错或猜一个状态好
}

var powerJobTaskOrdinal = map[int]string{
	1: protocol.TaskWaiting, // WAITING_DISPATCH
	2: protocol.TaskWaiting, // DISPATCH_SUCCESS_WORKER_UNCHECK
	3: protocol.TaskWaiting, // WORKER_RECEIVED
	4: protocol.TaskRunning, // WORKER_PROCESSING
	5: protocol.TaskFailed,  // WORKER_PROCESS_FAILED
	6: protocol.TaskSucceed, // WORKER_PROCESS_SUCCESS
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

func intList(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("status in (...) 里只能是数字，收到 %q。%s", p, queryHelp)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("status in (...) 是空的。%s", queryHelp)
	}
	return out, nil
}

func strList(s string) ([]string, error) {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) < 2 || p[0] != '\'' || p[len(p)-1] != '\'' {
			return nil, fmt.Errorf("task_name in (...) 里每个值都要用单引号包住，收到 %q。%s", p, queryHelp)
		}
		out = append(out, p[1:len(p)-1])
	}
	return out, nil
}

// detailPlus POST /openapi/instance/detailPlus：实例行 + 任务统计 + 一页子任务。
func (s *Server) detailPlus(c *gin.Context) {
	req, err := bind[protocol.InstanceDetailReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	inst, err := s.st.Get[store.JobInstance](req.InstanceId)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, "instance not found")
		return
	}
	resp := InstanceDetailResp{Instance: *inst, TaskStats: taskStats(s.st.DB, inst.ID)}

	where, args, order, qerr := parseTaskQuery(req.CustomQuery)
	if qerr != nil {
		// PowerJob 把解析失败抛成整个请求失败；这里改成照常返回实例信息 + 说明哪里写歪了，
		// 详情弹窗的主体是实例，不该因为一句过滤条件白屏。
		resp.QueryError = qerr.Error()
		writeOK(c, resp)
		return
	}
	q := s.st.DB.Model(&store.InstanceTask{}).Where("instance_id = ?", inst.ID)
	for i, w := range where {
		q = q.Where(w, args[i])
	}
	var total int64
	if e := q.Count(&total).Error; e != nil {
		writeErr(c, protocol.CodeErr, e.Error())
		return
	}
	var rows []store.InstanceTask
	if e := q.Order(order).Limit(taskInfoLimit).Find(&rows).Error; e != nil {
		writeErr(c, protocol.CodeErr, e.Error())
		return
	}
	resp.Total = int(total)
	resp.Tasks = make([]TaskInfoRow, 0, len(rows))
	for i := range rows {
		t := &rows[i]
		resp.Tasks = append(resp.Tasks, TaskInfoRow{
			TaskID: t.TaskKey, TaskName: t.TaskName, TaskContent: t.SubTaskContent,
			ProcessorAddr: t.WorkerId, FailedCnt: max(t.Attempts-1, 0),
			Status: t.Status, StatusStr: taskStatusCN[t.Status],
			CreatedTime: t.CreateTime, ModifiedTime: t.ModifyTime, Result: t.Result,
		})
	}
	writeOK(c, resp)
}

// taskStats 一次取全这个实例的任务行做计数：单实例的行数受 maxInstanceTasks 约束，
// 比开六条 COUNT 查询更省。
func taskStats(db *gorm.DB, instanceId uint64) TaskStats {
	var rows []store.InstanceTask
	st := TaskStats{}
	if err := db.Where("instance_id = ?", instanceId).Find(&rows).Error; err != nil {
		return st
	}
	for i := range rows {
		st.Total++
		switch rows[i].Status {
		case protocol.TaskWaiting:
			st.Waiting++
		case protocol.TaskRunning:
			st.Running++
		case protocol.TaskSucceed:
			st.Succeed++
		case protocol.TaskFailed:
			st.Failed++
		case protocol.TaskStopped:
			st.Stopped++
		case protocol.TaskDiscarded:
			st.Discarded++
		}
	}
	return st
}
