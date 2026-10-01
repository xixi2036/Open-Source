package main

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var padding = bytes.Repeat([]byte("x"), 2<<20)

// sendStats 汇总一段发送的结果。
type sendStats struct {
	mu       sync.Mutex
	lat      []int64 // 发送请求耗时（纳秒）
	ok       int64   // 服务端接受的条数
	errors   int64   // 失败条数（网络错误、非 2xx、结果里带 error）
	errKinds map[string]int64
}

func (s *sendStats) add(d time.Duration, errKind string) {
	s.mu.Lock()
	s.lat = append(s.lat, int64(d))
	if errKind == "" {
		s.ok++
	} else {
		s.errors++
		if s.errKinds == nil {
			s.errKinds = map[string]int64{}
		}
		s.errKinds[errKind]++
	}
	s.mu.Unlock()
}

// sendOlm 发一条单聊消息，返回耗时与错误类别（空串表示成功）。
func (w *world) sendOlm(ctx context.Context, from, to *user, phase int64, size int) (time.Duration, string) {
	seq := w.msgSeq.Add(1)
	body := makeBody(phase, seq, size, padding)
	payload := []byte(`{"messages":[{"to":"` + to.id + `","type":1,"body":"` + body + `","client_id":"lt-` + w.runID + `-` + fmt.Sprint(seq) + `"}]}`)
	start := time.Now()
	status, raw, err := w.call(ctx, "POST", "/v1/messages", from.token, payload)
	d := time.Since(start)
	switch {
	case err != nil:
		return d, "network"
	case status != http.StatusOK:
		return d, fmt.Sprintf("http_%d", status)
	case bytes.Contains(raw, []byte(`"error"`)):
		return d, "rejected:" + between(raw, []byte(`"error":"`))
	}
	return d, ""
}

// sendGroup 发一条群消息。
func (w *world) sendGroup(ctx context.Context, from *user, g *group, phase int64, size int) (time.Duration, string) {
	seq := w.msgSeq.Add(1)
	body := makeBody(phase, seq, size, padding)
	payload := []byte(`{"session_id":"lt-session","body":"` + body + `","client_id":"lt-` + w.runID + `-` + fmt.Sprint(seq) + `"}`)
	start := time.Now()
	status, _, err := w.call(ctx, "POST", "/v1/groups/"+g.id+"/messages", from.token, payload)
	d := time.Since(start)
	switch {
	case err != nil:
		return d, "network"
	case status != http.StatusOK:
		return d, fmt.Sprintf("http_%d", status)
	}
	return d, ""
}

// dmLoad 以 conc 个并发闭环发送单聊消息（随机发送方 → 其小圈子里的随机好友），直到 stop 关闭。
// rate>0 时总速率不超过 rate 条/秒。
func (w *world) dmLoad(ctx context.Context, conc, rate int, phase int64, size int, stop <-chan struct{}, st *sendStats) {
	var tokens chan struct{}
	if rate > 0 {
		tokens = make(chan struct{}, conc)
		go func() {
			tick := time.NewTicker(time.Second / time.Duration(rate))
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					select {
					case tokens <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	n := w.o.users
	var wg sync.WaitGroup
	for k := 0; k < conc; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if tokens != nil {
					select {
					case <-tokens:
					case <-stop:
						return
					}
				}
				from := w.users[rand.IntN(n)]
				if len(from.friends) == 0 {
					continue
				}
				to := w.users[from.friends[rand.IntN(len(from.friends))]]
				d, kind := w.sendOlm(ctx, from, to, phase, size)
				st.add(d, kind)
			}
		}()
	}
	wg.Wait()
}

// runDM：N 个用户全部在线，conc 个并发发送单聊。
func (w *world) runDM(ctx context.Context, conc int) result {
	o := w.o
	logf("单聊：%d 在线，发送并发 %d，%s，body %dB", o.users, conc, o.duration, o.size)
	phase := w.beginPhase()
	st := &sendStats{}
	stop := make(chan struct{})
	start := time.Now()
	time.AfterFunc(o.duration, func() { close(stop) })
	w.dmLoad(ctx, conc, o.rate, phase, o.size, stop, st)
	elapsed := time.Since(start)
	w.settle(st.ok, 15*time.Second)
	lat, dups := w.collect()
	r := result{
		Scenario: "dm", Name: fmt.Sprintf("单聊 %d 在线 × 并发 %d（%dB）", o.users, conc, o.size),
		Concurrency: conc, Seconds: elapsed.Seconds(), Sent: st.ok + st.errors, Errors: st.errors, ErrorKinds: st.errKinds,
		Expected: st.ok, Delivered: int64(len(lat)), Dups: dups,
		Throughput: float64(st.ok) / elapsed.Seconds(),
		Send:       summarize(st.lat), E2E: summarize(lat),
	}
	r.finish()
	logf("  → %.0f 条/秒，端到端 P50 %.2fms P95 %.2fms P99 %.2fms，错误 %d，丢失 %d，重复 %d",
		r.Throughput, r.E2E.P50, r.E2E.P95, r.E2E.P99, r.Errors, r.Lost, r.Dups)
	return r
}

// runGroup：群成员全部在线，conc 个并发轮流以不同成员的身份发群消息。
func (w *world) runGroup(ctx context.Context, g *group, size, conc int) result {
	o := w.o
	dur := min(o.duration, 10*time.Second)
	logf("群消息：%d 人群，body %dB，并发 %d，%s", len(g.members), size, conc, dur)
	phase := w.beginPhase()
	st := &sendStats{}
	var next atomic.Int64
	stop := time.Now().Add(dur)
	start := time.Now()
	var wg sync.WaitGroup
	for k := 0; k < conc; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				from := g.members[int(next.Add(1))%len(g.members)]
				d, kind := w.sendGroup(ctx, from, g, phase, size)
				st.add(d, kind)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	expected := st.ok * int64(len(g.members)-1)
	w.settle(expected, 30*time.Second)
	lat, dups := w.collect()
	r := result{
		Scenario: "group", Name: fmt.Sprintf("%d 人群 × %s（并发 %d）", len(g.members), human(size), conc),
		Concurrency: conc, Seconds: elapsed.Seconds(), Sent: st.ok + st.errors, Errors: st.errors, ErrorKinds: st.errKinds,
		Expected: expected, Delivered: int64(len(lat)), Dups: dups,
		Throughput: float64(st.ok) / elapsed.Seconds(),
		Send:       summarize(st.lat), E2E: summarize(lat),
	}
	r.finish()
	logf("  → %.1f 条群消息/秒（%.0f 次投递/秒），请求 P50 %.2fms P95 %.2fms，全体端到端 P50 %.2fms P95 %.2fms P99 %.2fms，错误 %d，丢失 %d",
		r.Throughput, r.Throughput*float64(len(g.members)-1), r.Send.P50, r.Send.P95, r.E2E.P50, r.E2E.P95, r.E2E.P99, r.Errors, r.Lost)
	return r
}

// probe 以 workers 个协程循环请求 path（每次间隔 gap），直到 stop 关闭；返回每次请求的耗时与失败次数。
func (w *world) probe(ctx context.Context, path string, users []*user, workers int, gap time.Duration, stop <-chan struct{}) (lat []int64, errs int64) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			var local []int64
			var localErr int64
			for i := 0; ; i++ {
				select {
				case <-stop:
					mu.Lock()
					lat = append(lat, local...)
					errs += localErr
					mu.Unlock()
					return
				default:
				}
				token := ""
				if len(users) > 0 {
					token = users[(k+i)%len(users)].token
				}
				start := time.Now()
				status, _, err := w.call(ctx, "GET", path, token, nil)
				local = append(local, int64(time.Since(start)))
				if err != nil || status != http.StatusOK {
					localErr++
				}
				time.Sleep(gap)
			}
		}(k)
	}
	wg.Wait()
	return lat, errs
}

// readers 返回用于读探针的普通用户（取末尾的一段，避开当群主、好友特别多的 0 号用户）。
func (w *world) readers() []*user {
	n := min(len(w.users), w.o.users)
	return w.users[max(0, n-50):n]
}

// runReads：读接口延迟；writeConc>0 时同时有这么多并发在发单聊。
func (w *world) runReads(ctx context.Context, writeConc int) result {
	o := w.o
	dur := min(o.duration, 10*time.Second)
	if writeConc > 0 {
		logf("读延迟：同时有 %d 个并发在发单聊，%s", writeConc, dur)
	} else {
		logf("读延迟：空闲时，%s", dur)
	}
	phase := w.beginPhase()
	st := &sendStats{}
	stop := make(chan struct{})
	time.AfterFunc(dur, func() { close(stop) })
	var wg sync.WaitGroup
	start := time.Now()
	if writeConc > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); w.dmLoad(ctx, writeConc, 0, phase, o.size, stop, st) }()
	}
	paths := []string{"/v1/me", "/v1/friends", "/v1/moments/feed"}
	out := make([]readStat, len(paths)+1)
	readers := w.readers()
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			lat, errs := w.probe(ctx, p, readers, 2, 2*time.Millisecond, stop)
			out[i] = readStat{Path: "GET " + p, Errors: errs, summary: summarize(lat)}
		}(i, p)
	}
	// 0 号用户是两个大群的群主、好友最多：它的动态流要归并几百个作者的动态
	wg.Add(1)
	go func() {
		defer wg.Done()
		lat, errs := w.probe(ctx, "/v1/moments/feed", w.users[:1], 1, 2*time.Millisecond, stop)
		out[len(paths)] = readStat{Path: "GET /v1/moments/feed（好友最多的用户）", Errors: errs, summary: summarize(lat)}
	}()
	wg.Wait()
	elapsed := time.Since(start)
	w.settle(st.ok, 10*time.Second)
	name := "读延迟（空闲）"
	if writeConc > 0 {
		name = fmt.Sprintf("读延迟（单聊写压，并发 %d）", writeConc)
	}
	r := result{Scenario: "reads", Name: name, Concurrency: writeConc, Seconds: elapsed.Seconds(),
		Sent: st.ok + st.errors, Errors: st.errors, ErrorKinds: st.errKinds, Throughput: float64(st.ok) / elapsed.Seconds(), Reads: out}
	lat, dups := w.collect()
	r.Expected, r.Delivered, r.Dups = st.ok, int64(len(lat)), dups
	r.Send, r.E2E = summarize(st.lat), summarize(lat)
	r.finish()
	for _, rs := range out {
		logf("  → %-40s P50 %.2fms P95 %.2fms P99 %.2fms 最大 %.2fms（%d 次，失败 %d）", rs.Path, rs.P50, rs.P95, rs.P99, rs.Max, rs.Count, rs.Errors)
	}
	return r
}

func dbSize(path string) int64 {
	if path == "" {
		return 0
	}
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if st, err := os.Stat(p); err == nil {
			n += st.Size()
		}
	}
	return n
}

// runStall：大群里逐条发大消息，同时用探针量其他请求的停顿（GET /v1/me 与 /healthz 的最大耗时）。
func (w *world) runStall(ctx context.Context, g *group, size, count int) result {
	logf("大群发大消息：%d 人群，body %s × %d 条（逐条发送），同时探测 GET /v1/me 与 /healthz", len(g.members), human(size), count)
	phase := w.beginPhase()
	st := &sendStats{}
	stop := make(chan struct{})
	readers := w.readers()
	if len(w.users) > len(g.members) {
		readers = w.users[len(g.members):min(len(w.users), len(g.members)+50)] // 不在这个群里的用户
	}
	var wg sync.WaitGroup
	out := make([]readStat, 2)
	for i, p := range []string{"/v1/me", "/healthz"} {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			us := readers
			if p == "/healthz" {
				us = nil
			}
			lat, errs := w.probe(ctx, p, us, 4, time.Millisecond, stop)
			out[i] = readStat{Path: "GET " + p, Errors: errs, summary: summarize(lat)}
		}(i, p)
	}
	time.Sleep(500 * time.Millisecond) // 先量一小段空闲基线
	before := dbSize(w.o.dbPath)
	start := time.Now()
	for i := 0; i < count; i++ {
		from := g.members[(i*7+1)%len(g.members)]
		d, kind := w.sendGroup(ctx, from, g, phase, size)
		st.add(d, kind)
		time.Sleep(200 * time.Millisecond)
	}
	elapsed := time.Since(start)
	expected := st.ok * int64(len(g.members)-1)
	w.settle(expected, 60*time.Second)
	after := dbSize(w.o.dbPath)
	close(stop)
	wg.Wait()
	lat, dups := w.collect()
	r := result{
		Scenario: "stall", Name: fmt.Sprintf("%d 人群逐条发 %s × %d", len(g.members), human(size), count),
		Concurrency: 1, Seconds: elapsed.Seconds(), Sent: st.ok + st.errors, Errors: st.errors, ErrorKinds: st.errKinds,
		Expected: expected, Delivered: int64(len(lat)), Dups: dups,
		Throughput: float64(st.ok) / elapsed.Seconds(),
		Send:       summarize(st.lat), E2E: summarize(lat), Reads: out,
	}
	if w.o.dbPath != "" {
		r.DBGrowthBytes = after - before
	}
	r.finish()
	logf("  → 发送请求 P50 %.1fms 最大 %.1fms；全体端到端 P95 %.1fms；期间 GET /v1/me P50 %.2fms P99 %.2fms 最大 %.1fms；/healthz 最大 %.1fms；库增长 %s；错误 %d，丢失 %d",
		r.Send.P50, r.Send.Max, r.E2E.P95, out[0].P50, out[0].P99, out[0].Max, out[1].Max, human(int(r.DBGrowthBytes)), r.Errors, r.Lost)
	return r
}

func human(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.4gMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.4gKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
