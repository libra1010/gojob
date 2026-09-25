package cluster

// 集群路由提示的数据来源：把"谁持有该 app 的租约"翻译成"worker 该把长轮询挂到哪台副本"。
//
// 只用既有的两张表做只读联查（app_leases 由 Leases 维护，server_nodes 由 node.Publisher
// 每 5s upsert），不新增状态，也不新增 -advertise-address 这类配置：
// server_nodes.public_url 语义本来就是"别人可达本副本的地址"，直接复用。

import (
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"

	"gojob/server/node"
	"gojob/server/store"
)

// Owner 返回该 app 当前的调度归属副本：holder 的 node_id，以及它对 worker 可达的地址。
//
// address 为空表示"没有可用提示"，这不是错误，调用方应照常工作（owner 只是首选地址，不承重）：
//  1. 租约行不存在或已过期——还没人接管，或单机部署根本没开租约；
//  2. holder 心跳已掉线（超过 node.OfflineAfter）——它可能已经死了，这时把 worker 支过去最危险，
//     宁可不提示，让 worker 留在原地靠长轮询兜底；
//  3. holder 那行 public_url 为空（漏配对外地址）——给不出地址就不给一个错的。
//
// nodeId 一律返回租约里的原始 holder，供日志与排障用（哪怕 address 不可用）。
func Owner(db *gorm.DB, appId string) (nodeId, address string, err error) {
	now := store.Now()
	var l store.AppLease
	if err := db.Where("app_id = ?", appId).First(&l).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", nil
		}
		return "", "", err
	}
	if l.Holder == "" {
		return "", "", nil
	}
	// 以下三种情况只关"地址"这一半，nodeId 照实返回：排障时要能看见"租约上写着谁"，
	// 而不是因为地址不可用就连身份一起抹掉。
	if !l.LeaseExpire.After(now) {
		return l.Holder, "", nil
	}
	var n store.ServerNode
	if err := db.Where("node_id = ?", l.Holder).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return l.Holder, "", nil
		}
		return "", "", err
	}
	if !aliveNode(n.SampleTime, now) || n.PublicURL == "" {
		return l.Holder, "", nil
	}
	return n.NodeId, n.PublicURL, nil
}

// AliveServers 在线副本对 worker 可达的地址列表（升序、去重、剔除没配对外地址的行）。
// 下发给 worker 当"已知地址集合"，容错与 owner 切换都只在这个集合里打转。
func AliveServers(db *gorm.DB) ([]string, error) {
	now := store.Now()
	var ns []store.ServerNode
	if err := db.Find(&ns).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(ns))
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		if !aliveNode(n.SampleTime, now) || n.PublicURL == "" || seen[n.PublicURL] {
			continue
		}
		seen[n.PublicURL] = true
		out = append(out, n.PublicURL)
	}
	sort.Strings(out)
	return out, nil
}

func aliveNode(sampleTime, now time.Time) bool {
	return sampleTime.After(now.Add(-node.OfflineAfter))
}
