package store

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// 列宽必须装得下调度类型与每日间隔表达式的最长取值：
// DAILY_TIME_INTERVAL 本身 19 字符，表达式是约 130 字符的 JSON。
// sqlite 不校验 VARCHAR 长度，只有 MySQL/PostgreSQL 会因此丢实例，所以用 schema 断言兜住。
func TestScheduleColumnWidths(t *testing.T) {
	const dailyExpr = `{"interval":30,"intervalUnit":"MILLISECONDS","startTimeOfDay":"09:00:00","endTimeOfDay":"18:00:00","daysOfWeek":[1,2,3,4,5,6,7]}`
	cases := []struct {
		model any
		field string
		value string
	}{
		{&JobInfo{}, "TimeExpression", dailyExpr},
		{&JobInstance{}, "Source", "DAILY_TIME_INTERVAL"},
	}
	for _, c := range cases {
		s, err := schema.Parse(c.model, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		f := s.LookUpField(c.field)
		if f == nil {
			t.Fatalf("%s 字段不存在", c.field)
		}
		if f.Size < len(c.value) {
			t.Fatalf("%s 列宽 %d 装不下最长取值 %q（%d 字符）", c.field, f.Size, c.value, len(c.value))
		}
	}
}
