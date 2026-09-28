package node

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"gojob/server/store"
)

// 假连接：只需要占位身份，ConnState 不碰它。
type fakeConn string

func (fakeConn) Read([]byte) (int, error)         { return 0, nil }
func (fakeConn) Write([]byte) (int, error)        { return 0, nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return nil }
func (fakeConn) RemoteAddr() net.Addr             { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestConnStateBookkeeping(t *testing.T) {
	c := New()
	a, b, d := fakeConn("a"), fakeConn("b"), fakeConn("d")

	c.ConnState(a, http.StateActive)
	c.ConnState(b, http.StateActive)
	c.ConnState(b, http.StateIdle) // 响应写完转空闲
	c.ConnState(d, http.StateHijacked)
	c.ConnState(d, http.StateClosed) // 关闭后不该再被计数

	s := c.Sample()
	if s.ConnsActive != 1 || s.ConnsIdle != 1 {
		t.Fatalf("连接数 = %d/%d, want active=1 idle=1", s.ConnsActive, s.ConnsIdle)
	}
}

func TestRequestCounters(t *testing.T) {
	c := New()
	c.EnterRequest()
	c.EnterPoll()
	s := c.Sample()
	if s.ReqInflight != 1 || s.PollInflight != 1 {
		t.Fatalf("在途 = req %d poll %d, want 1/1", s.ReqInflight, s.PollInflight)
	}
	c.LeavePoll()
	c.LeaveRequest()
	s = c.Sample()
	if s.ReqInflight != 0 || s.PollInflight != 0 {
		t.Fatalf("退出后在途应为 0，got %d/%d", s.ReqInflight, s.PollInflight)
	}
}

// 环形缓冲：写满后覆盖最旧，且 History() 必须按时间升序（前端直接拿来画曲线，顺序反了就错）。
func TestHistoryRing(t *testing.T) {
	c := New()
	for i := 0; i < HistoryLen+20; i++ {
		c.Sample()
	}
	h := c.History()
	if len(h) != HistoryLen {
		t.Fatalf("曲线点数 = %d, want %d", len(h), HistoryLen)
	}
	for i := 1; i < len(h); i++ {
		if h[i].Time < h[i-1].Time {
			t.Fatalf("第 %d 点时间倒流：%d < %d", i, h[i].Time, h[i-1].Time)
		}
	}
}

func TestSeriesRoundTrip(t *testing.T) {
	hist := []Sample{
		{Time: 1, CPUPercent: 12.34, RssMb: 100.5, GcCount: 7, ConnsActive: 3, ConnsIdle: 2, ReqPerSec: 8.88, PollInflight: 3},
		{Time: 2, CPUPercent: 0, GcCount: 8},
	}
	s := ParseSeries(seriesJSON(hist))
	if s == nil {
		t.Fatal("series 解析失败")
	}
	if len(s.Time) != 2 || s.CPU[0] != 12.3 || s.Mem[0] != 100.5 || s.Gc[1] != 8 {
		t.Fatalf("解析结果不对：%+v", s)
	}
	if s.Conns[0] != 5 || s.Poll[0] != 3 {
		t.Fatalf("连接列不对：%+v", s)
	}
	if ParseSeries("") != nil || ParseSeries("{坏数据") != nil {
		t.Fatal("脏数据应解析成 nil，让页面退化为只显示当前值")
	}
}

func TestPublisherUpsertsOneRowPerNode(t *testing.T) {
	st := openNodeStore(t)
	p := NewPublisher(st.DB, "node-1", "host-a", ":7789", "http://a", time.Now())
	s := Sample{Time: time.Now().UnixMilli(), CPUPercent: 7.5, Goroutines: 42, GcCount: 3, ConnsActive: 2}
	if err := p.Publish(s, []Sample{s}); err != nil {
		t.Fatal(err)
	}
	// 第二次改值再发：必须落在同一行（node_id 唯一），否则副本列表会越堆越长
	s2 := Sample{Time: s.Time, CPUPercent: 0, Goroutines: 7, GcCount: 4}
	if err := p.Publish(s2, []Sample{s, s2}); err != nil {
		t.Fatal(err)
	}
	var rows []store.ServerNode
	st.DB.Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("server_nodes 行数 = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.CPUPercent != 0 || r.Goroutines != 7 || r.GcCount != 4 {
		t.Fatalf("upsert 未覆盖旧值：%+v", r)
	}
	if r.Samples == "" {
		t.Fatal("采样序列没落库")
	}
}

// 数值列一旦带 gorm default 标签，零值就不写库（本项目踩过的坑）：
// CPU=0、连接=0 是完全正常的空闲值，必须能落下去。
func TestZeroValuesArePersisted(t *testing.T) {
	st := openNodeStore(t)
	p := NewPublisher(st.DB, "node-z", "host-z", ":7789", "", time.Now())
	if err := p.Publish(Sample{Time: time.Now().UnixMilli()}, nil); err != nil {
		t.Fatal(err)
	}
	var got store.ServerNode
	if err := st.DB.Where("node_id = ?", "node-z").First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.CPUPercent != 0 || got.RssMb != 0 || got.GcCount != 0 || got.Goroutines != 0 {
		t.Fatalf("零值没落库：%+v", got)
	}
}

func openNodeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}
