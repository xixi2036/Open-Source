// Package metrics 是一个轻量的进程内指标库：按路由的请求计数与耗时直方图、每秒请求数、
// 最近的 5xx / panic 记录，并能输出 Prometheus 文本格式。不依赖任何第三方库。
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Buckets 是耗时直方图的桶上界（秒）。
var Buckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const (
	windowSlots = 5  // 滑动窗口：最近 5 个整分钟（含当前分钟）
	rpsSlots    = 64 // 每秒请求数环形缓冲
	maxRecent   = 50 // 保留最近多少条 5xx / panic
	maxRoutes   = 512
)

type hist struct {
	counts []uint64 // len(Buckets)+1，最后一格是 +Inf
	sum    float64
	n      uint64
}

func (h *hist) observe(sec float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(Buckets)+1)
	}
	i := sort.SearchFloat64s(Buckets, sec)
	h.counts[i]++
	h.sum += sec
	h.n++
}

func (h *hist) add(o *hist) {
	if o.counts == nil {
		return
	}
	if h.counts == nil {
		h.counts = make([]uint64, len(Buckets)+1)
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.sum += o.sum
	h.n += o.n
}

// quantile 用桶内线性插值估算分位数（秒）；落在 +Inf 桶时返回最大桶上界。
func (h *hist) quantile(q float64) float64 {
	if h.n == 0 {
		return 0
	}
	rank := q * float64(h.n)
	var cum float64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		if cum+float64(c) >= rank {
			if i >= len(Buckets) {
				return Buckets[len(Buckets)-1]
			}
			lo := 0.0
			if i > 0 {
				lo = Buckets[i-1]
			}
			return lo + (Buckets[i]-lo)*(rank-cum)/float64(c)
		}
		cum += float64(c)
	}
	return Buckets[len(Buckets)-1]
}

// ahist 是可以并发写的直方图：每个桶一个原子计数器，热路径上不加锁。
type ahist struct {
	counts   [17]atomic.Uint64 // len(Buckets)+1，最后一格是 +Inf
	sumNanos atomic.Uint64
	n        atomic.Uint64
}

func (h *ahist) observe(d time.Duration) {
	i := sort.SearchFloat64s(Buckets, d.Seconds())
	h.counts[i].Add(1)
	h.sumNanos.Add(uint64(max(d, 0)))
	h.n.Add(1)
}

func (h *ahist) reset() {
	for i := range h.counts {
		h.counts[i].Store(0)
	}
	h.sumNanos.Store(0)
	h.n.Store(0)
}

// load 取一份普通直方图快照（各计数器分别读取，彼此之间可能差一两次观测，对监控无碍）。
func (h *ahist) load() hist {
	out := hist{counts: make([]uint64, len(Buckets)+1)}
	for i := range h.counts {
		out.counts[i] = h.counts[i].Load()
		out.n += out.counts[i]
	}
	out.sum = float64(h.sumNanos.Load()) / 1e9
	return out
}

type winSlot struct {
	minute atomic.Int64
	h      ahist
}

type routeStats struct {
	total   ahist
	codes   sync.Map // int → *atomic.Uint64
	win     [windowSlots]winSlot
	skipLat atomic.Bool
}

func (rs *routeStats) code(status int) *atomic.Uint64 {
	if v, ok := rs.codes.Load(status); ok {
		return v.(*atomic.Uint64)
	}
	v, _ := rs.codes.LoadOrStore(status, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

// Incident 是一条 5xx 或 panic 记录。
type Incident struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"` // "5xx" | "panic"
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

type secSlot struct {
	stamp atomic.Int64
	count atomic.Uint64
}

// Registry 保存全部指标。零值不可用，用 New 创建。
//
// 每个请求都要调用 Observe：路由表是 sync.Map，计数与直方图都是原子计数器，请求之间不争用任何锁。
// 只有低频的 Incident（5xx / panic 记录）用互斥锁。
type Registry struct {
	start time.Time

	routes    sync.Map // route → *routeStats
	nRoutes   atomic.Int64
	secs      [rpsSlots]secSlot
	errors5xx atomic.Uint64
	requests  atomic.Uint64
	counters  sync.Map // name → *atomic.Uint64

	mu     sync.Mutex
	recent []Incident
	panics uint64
}

// New 创建指标表。
func New() *Registry {
	return &Registry{start: time.Now()}
}

// Start 返回进程（指标表）启动时间。
func (r *Registry) Start() time.Time { return r.start }

func (r *Registry) route(name string) *routeStats {
	if v, ok := r.routes.Load(name); ok {
		return v.(*routeStats)
	}
	if r.nRoutes.Load() >= maxRoutes {
		name = "other"
		if v, ok := r.routes.Load(name); ok {
			return v.(*routeStats)
		}
	}
	v, loaded := r.routes.LoadOrStore(name, &routeStats{})
	if !loaded {
		r.nRoutes.Add(1)
	}
	return v.(*routeStats)
}

// Observe 记录一次请求。route 必须是路由模式（如 "GET /v1/users/{id}"），不能是原始路径；
// latency=false 时只计数不进耗时直方图（WebSocket 长连接）。
func (r *Registry) Observe(route string, status int, d time.Duration, latency bool) {
	now := time.Now()
	rs := r.route(route)
	rs.code(status).Add(1)
	r.requests.Add(1)
	if status >= 500 {
		r.errors5xx.Add(1)
	}
	sec := now.Unix()
	slot := &r.secs[int(sec%rpsSlots)]
	if old := slot.stamp.Load(); old != sec && slot.stamp.CompareAndSwap(old, sec) {
		slot.count.Store(0)
	}
	slot.count.Add(1)
	if !latency {
		rs.skipLat.Store(true)
		return
	}
	rs.total.observe(d)
	minute := sec / 60
	w := &rs.win[int(minute%windowSlots)]
	if old := w.minute.Load(); old != minute && w.minute.CompareAndSwap(old, minute) {
		w.h.reset()
	}
	w.h.observe(d)
}

// Incident 追加一条 5xx / panic 记录（只保留最近 maxRecent 条）。
func (r *Registry) Incident(in Incident) {
	if in.TS == 0 {
		in.TS = time.Now().UnixMilli()
	}
	if len(in.Detail) > 4000 {
		in.Detail = in.Detail[:4000] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if in.Kind == "panic" {
		r.panics++
	}
	r.recent = append(r.recent, in)
	if len(r.recent) > maxRecent {
		r.recent = r.recent[len(r.recent)-maxRecent:]
	}
}

// Add 给一个具名计数器加 n（如 "envelopes_enqueued"）。
func (r *Registry) Add(name string, n uint64) {
	v, ok := r.counters.Load(name)
	if !ok {
		v, _ = r.counters.LoadOrStore(name, new(atomic.Uint64))
	}
	v.(*atomic.Uint64).Add(n)
}

// RouteSnapshot 是某个路由的统计快照。分位数取自最近约 5 分钟的滑动窗口，单位毫秒。
type RouteSnapshot struct {
	Route       string  `json:"route"`
	Count       uint64  `json:"count"`
	Errors      uint64  `json:"errors"` // 5xx 次数
	WindowCount uint64  `json:"window_count"`
	P50         float64 `json:"p50_ms"`
	P95         float64 `json:"p95_ms"`
	P99         float64 `json:"p99_ms"`
	AvgMs       float64 `json:"avg_ms"`
}

// Snapshot 是指标的整体快照（监控页用）。
type Snapshot struct {
	UptimeSeconds int64             `json:"uptime_seconds"`
	Requests      uint64            `json:"requests"`
	Errors5xx     uint64            `json:"errors_5xx"`
	Panics        uint64            `json:"panics"`
	RPS           float64           `json:"rps"` // 最近 5 个完整秒的平均
	Routes        []RouteSnapshot   `json:"routes"`
	Recent        []Incident        `json:"recent"` // 新的在前
	Counters      map[string]uint64 `json:"counters"`
}

// Snapshot 生成快照。
func (r *Registry) Snapshot() Snapshot {
	now := time.Now()
	r.mu.Lock()
	panics := r.panics
	recent := append([]Incident(nil), r.recent...)
	r.mu.Unlock()
	s := Snapshot{UptimeSeconds: int64(now.Sub(r.start).Seconds()), Requests: r.requests.Load(), Errors5xx: r.errors5xx.Load(), Panics: panics,
		Routes: []RouteSnapshot{}, Recent: []Incident{}, Counters: map[string]uint64{}}
	sec := now.Unix()
	var sum uint64
	for k := int64(1); k <= 5; k++ {
		slot := &r.secs[int((sec-k)%rpsSlots)]
		if slot.stamp.Load() == sec-k {
			sum += slot.count.Load()
		}
	}
	s.RPS = float64(sum) / 5
	minute := sec / 60
	r.routes.Range(func(k, v any) bool {
		rs := v.(*routeStats)
		rt := RouteSnapshot{Route: k.(string)}
		rs.codes.Range(func(code, n any) bool {
			c := n.(*atomic.Uint64).Load()
			rt.Count += c
			if code.(int) >= 500 {
				rt.Errors += c
			}
			return true
		})
		var w hist
		for i := range rs.win {
			if m := rs.win[i].minute.Load(); m != 0 && minute-m < windowSlots {
				h := rs.win[i].h.load()
				w.add(&h)
			}
		}
		rt.WindowCount = w.n
		if w.n > 0 {
			rt.P50, rt.P95, rt.P99 = w.quantile(0.5)*1000, w.quantile(0.95)*1000, w.quantile(0.99)*1000
			rt.AvgMs = w.sum / float64(w.n) * 1000
		}
		s.Routes = append(s.Routes, rt)
		return true
	})
	sort.Slice(s.Routes, func(i, j int) bool {
		if s.Routes[i].Count != s.Routes[j].Count {
			return s.Routes[i].Count > s.Routes[j].Count
		}
		return s.Routes[i].Route < s.Routes[j].Route
	})
	for i := len(recent) - 1; i >= 0; i-- {
		s.Recent = append(s.Recent, recent[i])
	}
	r.counters.Range(func(k, v any) bool {
		s.Counters[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return s
}

// Gauge 是写入 Prometheus 输出的一个瞬时值。
type Gauge struct {
	Name  string
	Help  string
	Value float64
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WritePrometheus 以 Prometheus 文本格式（0.0.4）输出全部指标与额外的瞬时值。
func (r *Registry) WritePrometheus(w io.Writer, gauges []Gauge) {
	routes := map[string]*routeStats{}
	var names []string
	r.routes.Range(func(k, v any) bool {
		routes[k.(string)] = v.(*routeStats)
		names = append(names, k.(string))
		return true
	})
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# HELP hush_http_requests_total HTTP 请求总数（按路由模式与状态码）。\n# TYPE hush_http_requests_total counter\n")
	for _, n := range names {
		counts := map[int]uint64{}
		var codes []int
		routes[n].codes.Range(func(code, v any) bool {
			counts[code.(int)] = v.(*atomic.Uint64).Load()
			codes = append(codes, code.(int))
			return true
		})
		sort.Ints(codes)
		for _, c := range codes {
			fmt.Fprintf(&b, "hush_http_requests_total{route=\"%s\",code=\"%d\"} %d\n", escapeLabel(n), c, counts[c])
		}
	}
	b.WriteString("# HELP hush_http_request_duration_seconds HTTP 请求耗时直方图（不含 WebSocket 长连接）。\n# TYPE hush_http_request_duration_seconds histogram\n")
	for _, n := range names {
		total := routes[n].total.load()
		if total.n == 0 {
			continue
		}
		var cum uint64
		for i, ub := range Buckets {
			cum += total.counts[i]
			fmt.Fprintf(&b, "hush_http_request_duration_seconds_bucket{route=\"%s\",le=\"%g\"} %d\n", escapeLabel(n), ub, cum)
		}
		fmt.Fprintf(&b, "hush_http_request_duration_seconds_bucket{route=\"%s\",le=\"+Inf\"} %d\n", escapeLabel(n), total.n)
		fmt.Fprintf(&b, "hush_http_request_duration_seconds_sum{route=\"%s\"} %g\n", escapeLabel(n), total.sum)
		fmt.Fprintf(&b, "hush_http_request_duration_seconds_count{route=\"%s\"} %d\n", escapeLabel(n), total.n)
	}
	r.mu.Lock()
	panics := r.panics
	r.mu.Unlock()
	fmt.Fprintf(&b, "# HELP hush_panics_total 请求处理中发生的 panic 次数。\n# TYPE hush_panics_total counter\nhush_panics_total %d\n", panics)
	counters := map[string]uint64{}
	var cn []string
	r.counters.Range(func(k, v any) bool {
		counters[k.(string)] = v.(*atomic.Uint64).Load()
		cn = append(cn, k.(string))
		return true
	})
	sort.Strings(cn)
	for _, n := range cn {
		fmt.Fprintf(&b, "# TYPE hush_%s_total counter\nhush_%s_total %d\n", n, n, counters[n])
	}
	fmt.Fprintf(&b, "# HELP hush_uptime_seconds 进程运行时长。\n# TYPE hush_uptime_seconds gauge\nhush_uptime_seconds %g\n", time.Since(r.start).Seconds())
	for _, g := range gauges {
		if g.Help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", g.Name, g.Help)
		}
		fmt.Fprintf(&b, "# TYPE %s gauge\n%s %g\n", g.Name, g.Name, g.Value)
	}
	io.WriteString(w, b.String())
}
