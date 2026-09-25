// 节点监控接口：跨副本的调度中心进程指标 + 在线 worker 的 runtime 指标。
// PowerJob 控制台没有这一页（它只有首页时间卡与 worker 机器表），属本项目增强项。
package api

import (
	"time"

	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/cluster"
	"gojob/server/node"
	"gojob/server/store"
)

// serverView 一个调度中心副本。Samples 是最近 HistoryLen 个采样点（约 10 分钟）。
type serverView struct {
	NodeId     string    `json:"node_id"`
	Hostname   string    `json:"hostname"`
	Address    string    `json:"address"`
	PublicURL  string    `json:"public_url"`
	BootTime   time.Time `json:"boot_time"`
	SampleTime time.Time `json:"sample_time"`
	Online     bool      `json:"online"`
	SilentMs   int64     `json:"silent_ms"` // 距最后一次心跳多久，判"僵死副本"用
	// 调度归属：多副本下"谁在调度哪些项目"是排查重复触发/漏触发的第一手信息。
	OwnedApps    []string     `json:"owned_apps"`
	OwnedCount   int          `json:"owned_count"`
	CPUPercent   float64      `json:"cpu_percent"`
	RssMb        float64      `json:"rss_mb"`
	HeapInuseMb  float64      `json:"heap_inuse_mb"`
	Goroutines   int          `json:"goroutines"`
	GcCount      uint32       `json:"gc_count"`
	GcPauseMs    float64      `json:"gc_pause_ms"`
	ConnsActive  int          `json:"conns_active"`
	ConnsIdle    int          `json:"conns_idle"`
	PollInflight int          `json:"poll_inflight"`
	ReqInflight  int          `json:"req_inflight"`
	ReqPerSec    float64      `json:"req_per_sec"`
	Samples      *node.Series `json:"samples"`
}

// workerView 一个 worker 节点（在线与刚失联的都给出，失联行标 online=false）。
type workerView struct {
	protocol.WorkerInfo
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Online        bool      `json:"online"`
}

type nodesResp struct {
	IntervalMs     int64                   `json:"interval_ms"` // 采样/上报间隔，前端据此决定刷新与自动判活
	OfflineAfterMs int64                   `json:"offline_after_ms"`
	Self           string                  `json:"self"` // 处理本次请求的副本 node_id
	LeaseTtlMs     int64                   `json:"lease_ttl_ms"`
	Leases         []cluster.HolderSummary `json:"leases"` // 每个项目当前的调度归属
	Servers        []serverView            `json:"servers"`
	Workers        []workerView            `json:"workers"`
}

func (s *Server) systemNodes(c *gin.Context) {
	var rows []store.ServerNode
	if err := s.st.DB.Order("hostname asc, node_id asc").Find(&rows).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	// 租约视图：归属是"当前谁在调度"，所以只把**未过期**的租约算到某个副本头上。
	// 过期行仍然回给前端（alive=false），页面就能区分"没人调度"和"我看不清谁在调度"。
	ls, err := cluster.Summarize(s.st.DB)
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	held := make(map[string][]string)
	for _, h := range ls {
		if h.Alive && h.Holder != "" {
			held[h.Holder] = append(held[h.Holder], h.AppId)
		}
	}
	now := time.Now()
	out := nodesResp{
		IntervalMs:     node.ReportInterval.Milliseconds(),
		OfflineAfterMs: node.OfflineAfter.Milliseconds(),
		LeaseTtlMs:     cluster.LeaseTTL.Milliseconds(),
		Leases:         ls,
		Servers:        make([]serverView, 0, len(rows)),
	}
	// 副本表只做 upsert、不删行，所以重启过很多次会攒下死副本；
	// 顺手把静默超久的清掉，避免列表越用越长（阈值给到 7 天，不影响"离线但可能要回来"的排查）。
	var stale []uint64
	for _, r := range rows {
		if now.Sub(r.SampleTime) > 7*24*time.Hour {
			stale = append(stale, r.ID)
			continue
		}
		sv := serverView{
			NodeId: r.NodeId, Hostname: r.Hostname, Address: r.Address, PublicURL: r.PublicURL,
			BootTime: r.BootTime, SampleTime: r.SampleTime,
			Online:     !r.SampleTime.Before(now.Add(-node.OfflineAfter)),
			SilentMs:   now.Sub(r.SampleTime).Milliseconds(),
			CPUPercent: r.CPUPercent, RssMb: r.RssMb, HeapInuseMb: r.HeapInuseMb,
			Goroutines: r.Goroutines, GcCount: r.GcCount, GcPauseMs: r.GcPauseMs,
			ConnsActive: r.ConnsActive, ConnsIdle: r.ConnsIdle,
			PollInflight: r.PollInflight, ReqInflight: r.ReqInflight, ReqPerSec: r.ReqPerSec,
			Samples: node.ParseSeries(r.Samples),
		}
		sv.OwnedApps = held[r.NodeId]
		sv.OwnedCount = len(sv.OwnedApps)
		out.Servers = append(out.Servers, sv)
	}
	if len(stale) > 0 {
		if err := s.st.DB.Delete(&store.ServerNode{}, stale).Error; err != nil {
			writeErr(c, protocol.CodeErr, err.Error())
			return
		}
	}
	if s.leases != nil {
		out.Self = s.leases.Me()
	}
	for _, n := range s.reg.All() {
		out.Workers = append(out.Workers, workerView{WorkerInfo: n.WorkerInfo,
			LastHeartbeat: n.LastHeartbeat, Online: n.Alive})
	}
	writeOK(c, out)
}
