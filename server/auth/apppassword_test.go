package auth

import (
	"crypto/md5"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"gojob/server/store"
)

func newAppStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func insertPlaintextApp(t *testing.T, st *store.Store, appId, plain string) {
	t.Helper()
	now := store.Now()
	// 绕过 createApp 的哈希逻辑，模拟老库里的明文行
	if err := st.DB.Create(&store.AppInfo{AppId: appId, AppName: appId, Password: plain,
		CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestHashPasswordForm(t *testing.T) {
	hash, err := HashPassword("demo123")
	if err != nil {
		t.Fatal(err)
	}
	if !IsPasswordHashed(hash) {
		t.Fatalf("hash = %q, want bcrypt 形态", hash)
	}
	if !CheckPassword(hash, "demo123") {
		t.Fatal("bcrypt 校验应通过")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("错误口令不应通过")
	}
	if IsPasswordHashed("demo123") || IsPasswordHashed(hash[:59]) {
		t.Fatal("IsPasswordHashed 误判")
	}
}

// 项目口令在库里必须是 bcrypt 摘要，且整行不得出现明文或其 md5 等价物
// （曾经为 PowerJob authApp 存过 password_md5，该列已按"安全优先"删除）。
func TestAppRowsCarryNoPasswordEquivalents(t *testing.T) {
	st := newAppStore(t)
	insertPlaintextApp(t, st, "a1", "pw-one")
	sum := md5.Sum([]byte("pw-one"))
	md5Proof := hex.EncodeToString(sum[:])
	if _, err := UpgradeAppPasswords(st); err != nil {
		t.Fatal(err)
	}

	var raw struct {
		Password string
	}
	if err := st.DB.Table("app_infos").Where("app_id = ?", "a1").First(&raw).Error; err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"pw-one", md5Proof} {
		if strings.Contains(raw.Password, leak) {
			t.Fatalf("password 列含明文凭据 %q", leak)
		}
	}
}

func TestUpgradeAppPasswordsIsIdempotent(t *testing.T) {
	st := newAppStore(t)
	insertPlaintextApp(t, st, "a1", "pw-one")
	insertPlaintextApp(t, st, "a2", "pw-two")

	n, err := UpgradeAppPasswords(st)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if n != 2 {
		t.Fatalf("upgraded = %d, want 2", n)
	}

	want := map[string]string{"a1": "pw-one", "a2": "pw-two"}
	var apps []store.AppInfo
	st.DB.Order("id asc").Find(&apps)
	before := apps[0].Password
	for _, a := range apps {
		if !IsPasswordHashed(a.Password) {
			t.Fatalf("app %s password 仍是明文/非 bcrypt：%q", a.AppId, a.Password)
		}
		if !CheckPassword(a.Password, want[a.AppId]) {
			t.Fatalf("app %s 升级后原口令校验失败", a.AppId)
		}
	}

	// 再跑一次不得重复升级（否则会不断换 bcrypt salt，也可能把已哈希值当明文二次哈希）
	n2, err := UpgradeAppPasswords(st)
	if err != nil {
		t.Fatalf("upgrade again: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second run upgraded = %d, want 0（必须幂等）", n2)
	}
	var after store.AppInfo
	st.DB.Where("app_id = ?", "a1").First(&after)
	if after.Password != before {
		t.Fatal("幂等重跑不得改动已升级的行")
	}
}
