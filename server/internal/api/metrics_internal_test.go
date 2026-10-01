package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hush/server/internal/metrics"
)

// handler 里的 panic 被兜住：返回统一的 500 错误体、记一条 panic 记录，指标按路由模式计数。
func TestPanicRecoveryAndRouteMetrics(t *testing.T) {
	m := metrics.New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/boom/{id}", func(w http.ResponseWriter, r *http.Request) { panic("kaboom " + r.PathValue("id")) })
	mux.HandleFunc("GET /v1/fail", func(w http.ResponseWriter, r *http.Request) { fail(w, codeInternal, "x") })
	mux.HandleFunc("GET /v1/ok/{id}", func(w http.ResponseWriter, r *http.Request) { ok(w, obj{}) })
	h := logRequests(jsonFallback(mux), m)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if rec := get("/v1/boom/secret-id-123"); rec.Code != 500 || !strings.Contains(rec.Body.String(), `"error":"internal"`) {
		t.Fatalf("panic 应返回统一的 500: %d %s", rec.Code, rec.Body)
	}
	get("/v1/fail")
	get("/v1/ok/aaa")
	get("/v1/ok/bbb")
	get("/v1/unknown/path")
	s := m.Snapshot()
	if s.Requests != 5 || s.Panics != 1 || s.Errors5xx != 2 || len(s.Recent) != 3 {
		t.Fatalf("指标不对: %+v", s)
	}
	counts := map[string]uint64{}
	for _, r := range s.Routes {
		counts[r.Route] = r.Count
		if strings.Contains(r.Route, "aaa") || strings.Contains(r.Route, "secret") {
			t.Fatalf("指标标签不能含原始路径: %s", r.Route)
		}
	}
	if counts["GET /v1/ok/{id}"] != 2 || counts["GET /v1/boom/{id}"] != 1 || counts["unmatched"] != 1 {
		t.Fatalf("按路由模式计数不对: %v", counts)
	}
	kinds := map[string]int{}
	for _, in := range s.Recent {
		kinds[in.Kind]++
		if in.Kind == "panic" && !strings.Contains(in.Detail, "kaboom") {
			t.Fatalf("panic 记录应含 panic 值: %+v", in)
		}
	}
	if kinds["panic"] != 1 || kinds["5xx"] != 2 {
		t.Fatalf("5xx / panic 记录不对: %v", kinds)
	}
}
