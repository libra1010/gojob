package node

import (
	"math"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	v2 "encoding/json/v2"

	"gojob/server/store"
)

// ReportInterval 采样与跨副本上报间隔。15s（3 个周期）没心跳即判离线。
const ReportInterval = 5 * time.Second

// OfflineAfter 心跳判活阈值。
const OfflineAfter = 3 * ReportInterval

// Middleware 统计总请求数、在途请求数与长轮询在途数。
// 长轮询单独计：它是"活跃连接数"居高不下的主因，混在一起看不出是挂着的 worker 多还是请求多。
func (c *Collector) Middleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		poll := ctx.Request.URL.Path == "/worker/fetchTask"
		c.EnterRequest()
		if poll {
			c.EnterPoll()
		}
		defer func() {
			if poll {
				c.LeavePoll()
			}
			c.LeaveRequest()
		}()
		ctx.Next()
	}
}

// Publisher 把本进程快照写进 server_nodes（每个副本一行，按 node_id upsert）。
type Publisher struct {
	db        *gorm.DB
	nodeID    string
	hostname  string
	address   string
	publicURL string
	bootTime  time.Time
}

func NewPublisher(db *gorm.DB, nodeID, hostname, address, publicURL string, bootTime time.Time) *Publisher {
	return &Publisher{db: db, nodeID: nodeID, hostname: hostname, address: address,
		publicURL: publicURL, bootTime: bootTime}
}

// Publish 一次心跳。曲线数据只存画图要用的那几列并保留一位小数，
// 否则每 5s 往 TEXT 列写整条结构化序列，多副本下 IO 浪费得没必要。
func (p *Publisher) Publish(s Sample, hist []Sample) error {
	now := store.Now()
	row := store.ServerNode{
		NodeId: p.nodeID, Hostname: p.hostname, Address: p.address, PublicURL: p.publicURL,
		BootTime: p.bootTime, SampleTime: now,
		CPUPercent: round1(s.CPUPercent), RssMb: round1(s.RssMb), HeapInuseMb: round1(s.HeapInuseMb),
		Goroutines: s.Goroutines, GcCount: s.GcCount, GcPauseMs: round1(s.GcPauseMs),
		ConnsActive: s.ConnsActive, ConnsIdle: s.ConnsIdle,
		PollInflight: int(s.PollInflight), ReqInflight: int(s.ReqInflight), ReqPerSec: round1(s.ReqPerSec),
		Samples:    seriesJSON(hist),
		CreateTime: now, ModifyTime: now,
	}
	return p.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns(publishColumns),
	}).Create(&row).Error
}

var publishColumns = []string{"hostname", "address", "public_url", "boot_time", "sample_time",
	"cpu_percent", "rss_mb", "heap_inuse_mb", "goroutines", "gc_count", "gc_pause_ms",
	"conns_active", "conns_idle", "poll_inflight", "req_inflight", "req_per_sec",
	"samples", "modify_time"}

// Series 是 server_nodes.samples 列的存储形态：按列存稀疏数值，前端直接喂给 sparkline。
type Series struct {
	Time  []int64   `json:"t"`
	CPU   []float64 `json:"cpu"`
	Mem   []float64 `json:"mem"`
	Gc    []uint32  `json:"gc"`
	Conns []int     `json:"conns"`
	QPS   []float64 `json:"qps"`
	Poll  []int     `json:"poll"`
}

func seriesJSON(hist []Sample) string {
	s := Series{
		Time:  make([]int64, 0, len(hist)),
		CPU:   make([]float64, 0, len(hist)),
		Mem:   make([]float64, 0, len(hist)),
		Gc:    make([]uint32, 0, len(hist)),
		Conns: make([]int, 0, len(hist)),
		QPS:   make([]float64, 0, len(hist)),
		Poll:  make([]int, 0, len(hist)),
	}
	for _, x := range hist {
		s.Time = append(s.Time, x.Time)
		s.CPU = append(s.CPU, round1(x.CPUPercent))
		s.Mem = append(s.Mem, round1(x.RssMb))
		s.Gc = append(s.Gc, x.GcCount)
		s.Conns = append(s.Conns, x.ConnsActive+x.ConnsIdle)
		s.QPS = append(s.QPS, round1(x.ReqPerSec))
		s.Poll = append(s.Poll, x.PollInflight)
	}
	bs, err := v2.Marshal(s)
	if err != nil {
		return ""
	}
	return string(bs)
}

// ParseSeries 解出采样序列；库里是空串或脏数据时返回 nil（页面退化成只显示当前值）。
func ParseSeries(raw string) *Series {
	if raw == "" {
		return nil
	}
	var s Series
	if err := v2.Unmarshal([]byte(raw), &s); err != nil || len(s.Time) == 0 {
		return nil
	}
	return &s
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
