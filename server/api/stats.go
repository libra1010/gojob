// 统计概览：Dashboard 登录后的首页数据来源。
package api

import (
	"time"

	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/store"
)

func (s *Server) statsOverview(c *gin.Context) {
	resp := protocol.StatsOverviewResp{
		ServerAddress:   s.publicURL,
		DbDriver:        s.dbDriver,
		Workers:         []protocol.WorkerStat{},
		Apps:            []protocol.AppStat{},
		RecentInstances: []protocol.JobInstanceBrief{},
	}
	count := func(model any, where string, args ...any) int64 {
		var n int64
		q := s.st.DB.Model(model)
		if where != "" {
			q = q.Where(where, args...)
		}
		_ = q.Count(&n).Error
		return n
	}
	resp.TotalUsers = count(&store.UserInfo{}, "")
	resp.TotalApps = count(&store.AppInfo{}, "")
	resp.TotalJobs = count(&store.JobInfo{}, "")
	resp.EnabledJobs = count(&store.JobInfo{}, "enable = ?", true)

	// 项目卡片：job 数 + 在线 worker 数
	var apps []store.AppInfo
	_ = s.st.DB.Order("id asc").Limit(100).Find(&apps).Error
	for _, a := range apps {
		as := protocol.AppStat{AppId: a.AppId, AppName: a.AppName,
			CreateTime:  a.CreateTime.Format("2006-01-02 15:04:05"),
			WorkerCount: int64(len(s.reg.Workers(a.AppId)))}
		_ = s.st.DB.Model(&store.JobInfo{}).Where("app_id = ?", a.AppId).Count(&as.JobCount).Error
		resp.Apps = append(resp.Apps, as)
		for _, w := range s.reg.Workers(a.AppId) {
			resp.OnlineWorkers++
			resp.Workers = append(resp.Workers, protocol.WorkerStat{
				Address: w.Address, AppId: w.AppId, SdkType: w.SdkType,
				Version: w.Version, CpuLoad: w.CpuLoad, MemoryUsed: w.MemoryUsed,
			})
		}
	}

	// 今日实例分布
	today := time.Now().Format("2006-01-02") + " 00:00:00"
	resp.TodayTotal = count(&store.JobInstance{}, "create_time >= ?", today)
	resp.TodaySucceed = count(&store.JobInstance{}, "create_time >= ? AND status = ?", today, protocol.InstanceSucceed)
	resp.TodayFailed = count(&store.JobInstance{}, "create_time >= ? AND status = ?", today, protocol.InstanceFailed)
	resp.TodayRunning = count(&store.JobInstance{}, "create_time >= ? AND status IN ?", today,
		[]string{protocol.InstanceWaiting, protocol.InstanceRunning})
	if done := resp.TodaySucceed + resp.TodayFailed; done > 0 {
		resp.SuccessRate = float64(resp.TodaySucceed) / float64(done) * 100
	}

	var recent []store.JobInstance
	_ = s.st.DB.Order("id desc").Limit(10).Find(&recent).Error
	for _, i := range recent {
		resp.RecentInstances = append(resp.RecentInstances, protocol.JobInstanceBrief{
			InstanceId: i.ID, JobId: i.JobId, AppId: i.AppId, JobName: i.JobName,
			Status: i.Status, Source: i.Source,
			CreateTime: i.CreateTime.Format("2006-01-02 15:04:05"),
			ModifyTime: i.ModifyTime.Format("2006-01-02 15:04:05"),
		})
	}
	writeOK(c, resp)
}
