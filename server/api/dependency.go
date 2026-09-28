// 依赖触发（gojob 扩展，兼容协议用工作流 DAG 表达任务依赖，这里简化为"单上游、成功即触发"）：
// 下游任务记录 upstream_job_id，上游实例成功收尾时由 Dispatcher 回调 Scheduler.TriggerDependency。
// 本文件只负责保存期的校验：触发关系必须在写入前确认无环，否则运行期会互相拉起无限实例。
package api

import (
	"fmt"
	"strings"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// maxUpstreamChain 依赖链长度上限。正常配置不会触及，纯粹是防御：
// 万一历史数据里已经存在环（例如直接改库），保存时也不该死循环。
const maxUpstreamChain = 64

// validateDependencySchedule 时间类型与依赖配置严格对应（依赖触发是"一种调度方式"，
// 与 cron/固定频率并列，不是叠加在时间调度之外的第二通道）：
//   - 选了 DEPENDENCY 就必须有上游，否则任务既不被时间驱动也不被上游拉起；
//   - 其他时间类型不允许留下 upstream_job_id，否则会存在"页面上看不见却会触发"的悬空依赖。
func validateDependencySchedule(teType string, upstreamId uint64) error {
	isDep := strings.EqualFold(strings.TrimSpace(teType), protocol.SourceDependency)
	switch {
	case isDep && upstreamId == 0:
		return fmt.Errorf("时间类型为 %s（依赖触发）时必须选择上游任务", protocol.SourceDependency)
	case !isDep && upstreamId > 0:
		return fmt.Errorf("upstream_job_id 仅在时间类型为 %s（依赖触发）时可用", protocol.SourceDependency)
	}
	return nil
}

// validateUpstream 校验"本任务（jobId，新建传 0）把 upstreamId 当作依赖上游"是否合法：
// 上游必须存在、与本任务同属一个 app，且沿链向上回溯不能回到本任务（成环）。
func (s *Server) validateUpstream(jobId uint64, appId string, upstreamId uint64) error {
	if upstreamId == 0 {
		return nil
	}
	if upstreamId == jobId {
		return fmt.Errorf("任务不能依赖自己")
	}
	cur := upstreamId
	for depth := 0; ; depth++ {
		if depth >= maxUpstreamChain {
			return fmt.Errorf("依赖链超过 %d 层，疑似成环", maxUpstreamChain)
		}
		up, err := s.st.Get[store.JobInfo](cur)
		if err != nil {
			return fmt.Errorf("上游任务 %d 不存在", cur)
		}
		if up.AppId != appId {
			return fmt.Errorf("上游任务 %d 属于项目 %s，依赖触发只能同项目内", cur, up.AppId)
		}
		if up.ID == jobId {
			return fmt.Errorf("依赖成环：%d 的上游链最终回到了本任务 %d", upstreamId, jobId)
		}
		if up.UpstreamJobId == 0 {
			return nil
		}
		cur = up.UpstreamJobId
	}
}
