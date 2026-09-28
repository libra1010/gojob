// 任务日志通道：Worker 批量上报（at-least-once）+ Server 幂等去重存储 + 增量查询。
package api

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"gojob/internal/protocol"
	"gojob/server/store"
)

const (
	maxLogBatchEntries = 2000      // 单次上报条数上限
	maxLogEntryContent = 64 * 1024 // 单条日志内容上限（字节）
)

// reportLogs 数据面：POST /worker/reportLogs。
// (task_id, seq) 唯一索引 + ON CONFLICT DO NOTHING：重复上报被静默忽略，
// 因此 worker 只在拿到 code=0 的 ack 后才清理本地缓冲，失败无限重发也不会产生重复日志。
func (s *Server) reportLogs(c *gin.Context) {
	req, err := bind[protocol.ReportLogsReq](c)
	if err != nil || req.AppId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if err := s.workerAuthorized(req.AppId, req.InstanceId, req.Token); err != nil {
		writeErr(c, protocol.CodeAuthErr, err.Error())
		return
	}
	if len(req.Entries) == 0 {
		writeOK(c, 0)
		return
	}
	if len(req.Entries) > maxLogBatchEntries {
		writeErr(c, protocol.CodeBadReq, "log batch too large")
		return
	}
	// 只接受属于本 app 任务的日志，防止跨 app 注入
	ids := make([]uint64, 0, len(req.Entries))
	for _, e := range req.Entries {
		ids = append(ids, e.TaskId)
	}
	var owned []uint64
	if err := s.st.DB.Model(&store.InstanceTask{}).
		Where("id IN ? AND app_id = ?", ids, req.AppId).
		Pluck("id", &owned).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	allowed := make(map[uint64]bool, len(owned))
	for _, id := range owned {
		allowed[id] = true
	}
	now := store.Now()
	rows := make([]store.InstanceTaskLog, 0, len(req.Entries))
	for _, e := range req.Entries {
		if !allowed[e.TaskId] || e.Seq == 0 {
			continue
		}
		content := e.Content
		if len(content) > maxLogEntryContent {
			content = content[:maxLogEntryContent] + "...(truncated)"
		}
		rows = append(rows, store.InstanceTaskLog{
			TaskId: e.TaskId, InstanceId: e.InstanceId, Seq: e.Seq,
			Level: e.Level, LogTime: e.Time, Content: content, CreateTime: now,
		})
	}
	if len(rows) > 0 {
		if err := s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).
			CreateInBatches(&rows, 500).Error; err != nil {
			writeErr(c, protocol.CodeErr, err.Error())
			return
		}
	}
	writeOK(c, len(rows))
}

// instanceTasks 查询某实例的全部任务（日志页先列任务再拉日志）。
func (s *Server) instanceTasks(c *gin.Context) {
	id, err := parseID(c.Query("instance_id"))
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	var tasks []store.InstanceTask
	if err := s.st.DB.Where("instance_id = ?", id).Order("id asc").Find(&tasks).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, tasks)
}

// instanceLogs 增量拉取任务日志：?task_id=&after_seq=0&limit=500
func (s *Server) instanceLogs(c *gin.Context) {
	taskId, err := parseID(c.Query("task_id"))
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	after, _ := parseUintOr(c.Query("after_seq"), 0)
	limit, _ := parseUintOr(c.Query("limit"), 500)
	limit = min(max(limit, 1), 2000)
	var logs []store.InstanceTaskLog
	if err := s.st.DB.Where("task_id = ? AND seq > ?", taskId, after).
		Order("seq asc").Limit(int(limit)).Find(&logs).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, logs)
}

func parseUintOr(q string, def uint64) (uint64, error) {
	if q == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(q, 10, 64)
	if err != nil {
		return def, err
	}
	return v, nil
}
