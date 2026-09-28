// owner 提示的下发口径：租约持有者要"租约没过期 + 心跳没掉线 + 配了对外地址"三个条件同时成立
// 才能作为提示给 worker；任何一条不满足都宁可回"没有提示"，因为错的 owner 比没有 owner 危险。
package cluster

import (
	"testing"
	"time"

	"gojob/server/node"
	"gojob/server/store"
)

func seedServerNode(t *testing.T, st *store.Store, nodeId, publicURL string, silent time.Duration) {
	t.Helper()
	now := store.Now()
	ts := now.Add(-silent)
	if err := st.DB.Create(&store.ServerNode{NodeId: nodeId, Hostname: nodeId, Address: ":7700",
		PublicURL: publicURL, BootTime: ts, SampleTime: ts, CreateTime: ts, ModifyTime: ts}).Error; err != nil {
		t.Fatal(err)
	}
}

func seedLease(t *testing.T, st *store.Store, appId, holder string, expireIn time.Duration, epoch int64) {
	t.Helper()
	now := store.Now()
	if err := st.DB.Create(&store.AppLease{AppId: appId, Holder: holder, Epoch: epoch,
		LeaseExpire: now.Add(expireIn), CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestOwnerResolvesToHolderAddress(t *testing.T) {
	st := newLeaseDB(t)
	seedServerNode(t, st, "srv-a", "http://10.0.0.1:7700", time.Second)
	seedLease(t, st, "app1", "srv-a", 5*time.Second, 1)

	nodeId, addr, err := Owner(st.DB, "app1")
	if err != nil {
		t.Fatal(err)
	}
	if nodeId != "srv-a" || addr != "http://10.0.0.1:7700" {
		t.Fatalf("owner = (%q,%q), want (srv-a, http://10.0.0.1:7700)", nodeId, addr)
	}
}

// 三种"给不出地址"的情形：宁缺毋滥。nodeId 照实返回，方便排障看清"租约上写着谁"。
func TestOwnerWithoutUsableAddress(t *testing.T) {
	cases := []struct {
		name   string
		appId  string
		holder string
		url    string
		expire time.Duration
		silent time.Duration
	}{
		{"租约已过期", "app1", "srv-a", "http://a:7700", -time.Second, time.Second},
		{"持有者心跳掉了", "app2", "srv-b", "http://b:7700", time.Minute, node.OfflineAfter + time.Minute},
		{"持有者没配对外地址", "app3", "srv-c", "", time.Minute, time.Second},
	}
	st := newLeaseDB(t)
	for _, tc := range cases {
		seedServerNode(t, st, tc.holder, tc.url, tc.silent)
		seedLease(t, st, tc.appId, tc.holder, tc.expire, 1)
		t.Run(tc.name, func(t *testing.T) {
			nodeId, addr, err := Owner(st.DB, tc.appId)
			if err != nil {
				t.Fatal(err)
			}
			if addr != "" {
				t.Fatalf("不该给出 owner 地址，实得 %q", addr)
			}
			if nodeId != tc.holder {
				t.Fatalf("nodeId = %q, want 原始 holder %q（租约内容不该被粉饰）", nodeId, tc.holder)
			}
		})
	}
}

// 没有租约行 = 单机部署或还没人接管：空提示，不是错误。
func TestOwnerNoLeaseRow(t *testing.T) {
	st := newLeaseDB(t)
	nodeId, addr, err := Owner(st.DB, "ghost")
	if err != nil {
		t.Fatalf("租约不存在不该报错: %v", err)
	}
	if nodeId != "" || addr != "" {
		t.Fatalf("= (%q,%q), want 全空", nodeId, addr)
	}
}

func TestAliveServersSkipsStaleAndUnaddressed(t *testing.T) {
	st := newLeaseDB(t)
	seedServerNode(t, st, "srv-b", "http://10.0.0.2:7700", time.Second)
	seedServerNode(t, st, "srv-a", "http://10.0.0.1:7700", 2*time.Second)
	seedServerNode(t, st, "srv-dead", "http://10.0.0.9:7700", node.OfflineAfter+time.Minute)
	seedServerNode(t, st, "srv-nourl", "", time.Second)
	seedServerNode(t, st, "srv-dup", "http://10.0.0.1:7700", time.Second) // 同地址（同机两进程共用对外地址）

	got, err := AliveServers(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://10.0.0.1:7700", "http://10.0.0.2:7700"}
	if len(got) != len(want) {
		t.Fatalf("AliveServers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AliveServers = %v, want %v（升序去重、剔除掉线与没配地址的副本）", got, want)
		}
	}
}
