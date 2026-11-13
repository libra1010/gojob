package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// 线上数据库账号只有增删改查权限，没有 ALTER/CREATE：auto-migrate=false 这条路径
// 必须既不碰结构、又能在启动期点名缺了什么，而不是等运行期报 Unknown column。
func TestPrepareSchemaRejectsUnbuiltDB(t *testing.T) {
	st, err := Open("sqlite", filepath.Join(t.TempDir(), "prod.db"))
	if err != nil {
		t.Fatal(err)
	}
	err = st.PrepareSchema()
	if err == nil {
		t.Fatal("空库上 PrepareSchema 应报错，不能默默起来")
	}
	for _, want := range []string{"app_infos", "job_infos", "scripts/ddl", "GOJOB_DB_AUTO_MIGRATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息缺少 %q，实得\n%v", want, err)
		}
	}
	// 负向验证：报错之后库里仍然一张表都没有，说明这条路径真的没执行 DDL。
	if st.DB.Migrator().HasTable(&AppInfo{}) {
		t.Fatal("PrepareSchema 不该建表")
	}
}

// 只建了一半的库要能报到具体缺哪一列。
func TestPrepareSchemaReportsMissingColumns(t *testing.T) {
	st, err := Open("sqlite", filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Exec(
		"CREATE TABLE app_infos (id integer primary key, app_id text)").Error; err != nil {
		t.Fatal(err)
	}
	err = st.PrepareSchema()
	if err == nil {
		t.Fatal("缺表的库上 PrepareSchema 应报错")
	}
	if !strings.Contains(err.Error(), "app_infos.token_epoch") {
		t.Fatalf("应点名缺的列，实得\n%v", err)
	}
}

// 建好的库上 PrepareSchema 必须放行（回填是 DML，增删改查权限够用）。
func TestPrepareSchemaPassesOnMigratedDB(t *testing.T) {
	st, err := Open("sqlite", filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.PrepareSchema(); err != nil {
		t.Fatalf("结构齐全时不该报错，实得 %v", err)
	}
}
