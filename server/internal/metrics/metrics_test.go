package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistogramQuantilesAndPrometheus(t *testing.T) {
	r := New()
	for i := 0; i < 90; i++ {
		r.Observe("GET /v1/me", 200, 2*time.Millisecond, true)
	}
	for i := 0; i < 9; i++ {
		r.Observe("GET /v1/me", 200, 80*time.Millisecond, true)
	}
	r.Observe("GET /v1/me", 500, 3*time.Second, true)
	r.Observe("GET /v1/ws", 101, time.Hour, false)
	r.Incident(Incident{Kind: "5xx", Method: "GET", Path: "/v1/me", Status: 500})
	r.Incident(Incident{Kind: "panic", Method: "GET", Path: "/v1/me", Status: 500, Detail: "boom"})
	r.Add("envelopes_enqueued", 7)

	s := r.Snapshot()
	if s.Requests != 101 || s.Errors5xx != 1 || s.Panics != 1 || len(s.Recent) != 2 || s.Recent[0].Kind != "panic" || s.Counters["envelopes_enqueued"] != 7 {
		t.Fatalf("快照不对: %+v", s)
	}
	var me RouteSnapshot
	for _, rt := range s.Routes {
		if rt.Route == "GET /v1/me" {
			me = rt
		}
		if rt.Route == "GET /v1/ws" && (rt.WindowCount != 0 || rt.Count != 1) {
			t.Fatalf("WS 只计数不进直方图: %+v", rt)
		}
	}
	// 90% 的请求在 1–2.5ms 桶，P50 应落在该桶内；P95 在 50–100ms 桶；P99 同样；分位数单调
	if me.Count != 100 || me.Errors != 1 || me.P50 < 1 || me.P50 > 2.5 || me.P95 < 50 || me.P95 > 100 || me.P99 < me.P95 {
		t.Fatalf("分位数不对: %+v", me)
	}
	var buf bytes.Buffer
	r.WritePrometheus(&buf, []Gauge{{Name: "hush_ws_connections", Help: "在线", Value: 3}})
	out := buf.String()
	for _, want := range []string{
		`hush_http_requests_total{route="GET /v1/me",code="200"} 99`,
		`hush_http_requests_total{route="GET /v1/me",code="500"} 1`,
		`hush_http_request_duration_seconds_bucket{route="GET /v1/me",le="0.0025"} 90`,
		`hush_http_request_duration_seconds_bucket{route="GET /v1/me",le="0.1"} 99`,
		`hush_http_request_duration_seconds_bucket{route="GET /v1/me",le="+Inf"} 100`,
		`hush_http_request_duration_seconds_count{route="GET /v1/me"} 100`,
		"hush_panics_total 1", "hush_envelopes_enqueued_total 7", "# TYPE hush_ws_connections gauge\nhush_ws_connections 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `route="GET /v1/ws",le=`) {
		t.Fatal("WS 不应有直方图")
	}
}

func TestConcurrentObserve(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Observe("GET /x", 200, time.Millisecond, true)
				r.Snapshot()
			}
		}()
	}
	wg.Wait()
	if s := r.Snapshot(); s.Requests != 5000 {
		t.Fatalf("计数不对: %d", s.Requests)
	}
}
