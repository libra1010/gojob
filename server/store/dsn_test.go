package store

import (
	"strings"
	"testing"
)

// 驱动与 DSN 不匹配必须在启动时点名，而不是把文件路径交给 MySQL/PG 驱动后
// 得到 "default addr for network '/app/data' unknown" 这种谜语。
func TestValidateDSNRejectsMismatch(t *testing.T) {
	cases := []struct{ driver, dsn string }{
		{"mysql", "/app/data/gojob.db"},
		{"mysql", "gojob.db"},
		{"postgres", "/app/data/gojob.db"},
		{"postgres", "./gojob.db"},
	}
	for _, c := range cases {
		err := validateDSN(c.driver, c.dsn)
		if err == nil {
			t.Fatalf("%s + %q 应被拒绝", c.driver, c.dsn)
		}
		if !strings.Contains(err.Error(), "GOJOB_DB_DSN") {
			t.Fatalf("错误信息应点名 GOJOB_DB_DSN，实得 %v", err)
		}
	}
}

func TestValidateDSNAcceptsValid(t *testing.T) {
	cases := []struct{ driver, dsn string }{
		{"sqlite", "/app/data/gojob.db"},
		{"mysql", "gojob:p@tcp(mysql:3306)/gojob?charset=utf8mb4&parseTime=True&loc=UTC"},
		{"mysql", "gojob:p@unix(/var/run/mysqld/mysqld.sock)/gojob"},
		{"postgres", "postgres://gojob:p@postgres:5432/gojob?sslmode=disable"},
		{"postgres", "host=postgres dbname=gojob user=gojob password=p"},
		{"postgres", "host=/var/run/postgresql dbname=gojob"},
	}
	for _, c := range cases {
		if err := validateDSN(c.driver, c.dsn); err != nil {
			t.Fatalf("%s + %q 应通过，实得 %v", c.driver, c.dsn, err)
		}
	}
}
