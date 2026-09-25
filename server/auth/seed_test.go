// 首启播种在并发下必须安全：多副本同时首次启动是常态部署场景，
// 谁输了竞争谁就当作"别人已经播过了"，而不是报错退出。
package auth

import (
	"sync"
	"testing"

	"gojob/server/store"
)

func TestSeedAdminConcurrentBoot(t *testing.T) {
	st := newAppStore(t)
	const replicas = 8
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	created := make([]bool, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created[i], errs[i] = SeedAdmin(st, "admin", "admin123")
		}(i)
	}
	wg.Wait()
	n := 0
	for i := 0; i < replicas; i++ {
		if errs[i] != nil {
			t.Fatalf("副本 %d 播种失败：%v（并发首启不允许有任何一方报错）", i, errs[i])
		}
		if created[i] {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("自称播种成功的副本有 %d 个, want 1（只有一个能真的写下这一行）", n)
	}
	var users []store.UserInfo
	st.DB.Find(&users)
	if len(users) != 1 {
		t.Fatalf("user_infos 有 %d 行, want 1", len(users))
	}
	// 播种确实可用：口令校验得过
	if !CheckPassword(users[0].Password, "admin123") {
		t.Fatal("播种出的账号口令校验失败")
	}
}
