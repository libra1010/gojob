// Package registry 维护 Worker 心跳形成的在线节点表。
//
// 落 DB 而不是留在进程内存里，是为了多副本下每个副本看到同一份候选集：
// 分发策略（健康优先/指定机器/轮询）与节点监控若只看"轮询过我的那些节点"，
// LB 后面的副本就会各拿一份残缺视图去选机器。
// 接口与内存版时期保持一致（Alive/Workers/All/Evict），只是实现换掉了。
package registry

import (
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gojob/internal/protocol"
	"gojob/server/store"
)

type Registry struct {
	db  *gorm.DB
	ttl time.Duration
}

func New(db *gorm.DB, ttl time.Duration) *Registry {
	return &Registry{db: db, ttl: ttl}
}

// Alive 记下一次心跳（upsert 本节点那一行）。
// 返回错误让心跳 handler 能把"没记上"如实回给 worker，而不是假装注册成功。
func (r *Registry) Alive(w protocol.WorkerInfo) error {
	now := store.Now()
	row := toRow(w, now)
	err := r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_id"}, {Name: "instance_id"}},
		DoUpdates: clause.AssignmentColumns(heartbeatColumns),
	}).Create(&row).Error
	if err != nil {
		log.Printf("registry: record heartbeat of %s/%s: %v", w.AppId, w.InstanceId, err)
	}
	return err
}

// heartbeatColumns 心跳要覆盖的列。app_id/instance_id 是定位键，create_time 不属于这里
// （upsert 更新时不能把首次上线时间刷掉）。
var heartbeatColumns = []string{"address", "cpu_load", "cpu_processors", "memory_used",
	"memory_max", "tag", "version", "sdk_type", "cpu_percent", "gc_count", "gc_pause_ms",
	"rss_mb", "threads", "open_fds", "running_tasks", "last_heartbeat", "modify_time"}

// Workers 返回某 app 全部存活节点（按 address 排序，供可插拔负载均衡策略使用）。
//
// 查询失败时返回空集合并记日志：调用方（分发策略）会退化成"没有可选节点"，
// 这比拿一份不知道新不新的旧数据去选机器更容易发现。
func (r *Registry) Workers(appId string) []protocol.WorkerInfo {
	var rows []store.WorkerNode
	if err := r.db.Where("app_id = ? AND last_heartbeat > ?", appId, r.deadline()).
		Order("address asc").Find(&rows).Error; err != nil {
		log.Printf("registry: list workers of %s: %v", appId, err)
		return nil
	}
	out := make([]protocol.WorkerInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, infoOf(row))
	}
	return out
}

// HasWorkers 认领热路径上的轻量判断：只数行数，不把整表节点读回来。
func (r *Registry) HasWorkers(appId string) (bool, error) {
	var n int64
	err := r.db.Model(&store.WorkerNode{}).
		Where("app_id = ? AND last_heartbeat > ?", appId, r.deadline()).Count(&n).Error
	return n > 0, err
}

// NodeSnapshot 一条在线节点记录，附带最后心跳时间与存活标记，供节点监控页直接展示。
type NodeSnapshot struct {
	protocol.WorkerInfo
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Alive         bool      `json:"alive"`
}

// All 跨项目全量快照（含已失联但还没被 Evict 的节点），按 app_id+address 排序。
// 分发策略用的是 Workers(appId)/HasWorkers，这里只给监控面用，两者不要混用。
func (r *Registry) All() []NodeSnapshot {
	var rows []store.WorkerNode
	if err := r.db.Order("app_id asc, address asc").Find(&rows).Error; err != nil {
		log.Printf("registry: list all nodes: %v", err)
		return nil
	}
	deadline := r.deadline()
	out := make([]NodeSnapshot, 0, len(rows))
	for _, row := range rows {
		out = append(out, NodeSnapshot{
			WorkerInfo:    infoOf(row),
			LastHeartbeat: row.LastHeartbeat,
			Alive:         row.LastHeartbeat.After(deadline),
		})
	}
	return out
}

// Evict 清理长期失联节点（10 个 TTL 没心跳就认为它不会再回来），由后台维护循环调用。
func (r *Registry) Evict() {
	cutoff := store.Now().Add(-10 * r.ttl)
	res := r.db.Where("last_heartbeat < ?", cutoff).Delete(&store.WorkerNode{})
	if res.Error != nil {
		log.Printf("registry: evict dead nodes: %v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("registry: evicted %d dead worker node(s)", res.RowsAffected)
	}
}

// deadline 存活判定的时间下限：心跳早于此即视为失联。
func (r *Registry) deadline() time.Time { return store.Now().Add(-r.ttl) }

func toRow(w protocol.WorkerInfo, now time.Time) store.WorkerNode {
	return store.WorkerNode{
		AppId: w.AppId, InstanceId: w.InstanceId, Address: w.Address,
		CpuLoad: w.CpuLoad, CpuProcessors: w.CpuProcessors,
		MemoryUsed: w.MemoryUsed, MemoryMax: w.MemoryMax,
		Tag: w.Tag, Version: w.Version, SdkType: w.SdkType,
		CpuPercent: w.CpuPercent, GcCount: w.GcCount, GcPauseMs: w.GcPauseMs,
		RssMb: w.RssMb, Threads: w.Threads, OpenFDs: w.OpenFDs,
		RunningTasks:  w.RunningTasks,
		LastHeartbeat: now, CreateTime: now, ModifyTime: now,
	}
}

// infoOf 把库里的节点行还原成协议结构（心跳与分发用的是同一份字段，不另造模型）。
func infoOf(n store.WorkerNode) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		InstanceId: n.InstanceId, AppId: n.AppId, Address: n.Address,
		CpuLoad: n.CpuLoad, CpuProcessors: n.CpuProcessors,
		MemoryUsed: n.MemoryUsed, MemoryMax: n.MemoryMax, Tag: n.Tag,
		Version: n.Version, SdkType: n.SdkType,
		CpuPercent: n.CpuPercent, GcCount: n.GcCount, GcPauseMs: n.GcPauseMs,
		RssMb: n.RssMb, Threads: n.Threads, OpenFDs: n.OpenFDs,
		RunningTasks: n.RunningTasks,
	}
}
