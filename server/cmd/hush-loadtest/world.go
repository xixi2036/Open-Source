package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// user 是一个模拟用户：一个账号、一条 WebSocket、若干小圈子好友。
type user struct {
	idx     int
	id      string
	token   string
	friends []int // 小圈子里的好友（world.users 的下标）

	ws *websocket.Conn

	mu      sync.Mutex
	phase   int64
	lat     []int64             // 当前阶段收到的信封的端到端延迟（纳秒）
	seen    map[uint64]struct{} // 当前阶段已收到的消息序号（判重）
	dups    int64
	pending []string // 待 ack 的 id

	events     atomic.Int64 // 收到的事件数
	groupEvent atomic.Int64 // 收到的 group_updated 数
	ready      chan struct{}
}

type world struct {
	o     options
	hc    *http.Client
	runID string
	users []*user

	phase  atomic.Int64
	msgSeq atomic.Uint64
	closed atomic.Bool

	setupInfo setupInfo
	wsErrors  atomic.Int64
}

type setupInfo struct {
	Users       int     `json:"users"`
	Circle      int     `json:"circle"`
	RegisterSec float64 `json:"register_seconds"`
	FriendSec   float64 `json:"friend_seconds"`
	ConnectSec  float64 `json:"connect_seconds"`
	Online      int     `json:"online"`
}

func newWorld(o options) *world {
	tr := &http.Transport{
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	b := make([]byte, 4)
	rand.Read(b)
	return &world{o: o, hc: &http.Client{Transport: tr, Timeout: 120 * time.Second}, runID: hex.EncodeToString(b)}
}

// call 发起一次 JSON 请求。body 为 []byte 时原样发送。返回状态码与响应体。
func (w *world) call(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.o.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// must 调用并要求 2xx；否则退出（建场景阶段的失败没有继续的意义）。
func (w *world) must(ctx context.Context, method, path, token string, body any, out any) {
	var status int
	var raw []byte
	var err error
	// 建场景阶段遇到限速 / 过载就退避重试，其他错误直接退出
	for attempt := 0; attempt < 8; attempt++ {
		status, raw, err = w.call(ctx, method, path, token, body)
		if err == nil && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		fatalf("%s %s: %v", method, path, err)
	}
	if status/100 != 2 {
		fatalf("%s %s: HTTP %d %s", method, path, status, truncate(raw, 300))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			fatalf("%s %s: 解析响应失败: %v", method, path, err)
		}
	}
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// parallel 以 conc 个并发对 [0,n) 执行 fn。
func parallel(n, conc int, fn func(i int)) {
	var wg sync.WaitGroup
	var next atomic.Int64
	for k := 0; k < min(conc, n); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

func fakeKey(tag string, i, j int) string {
	return fmt.Sprintf("%s%06d%04dAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", tag, i, j)
}

// setup 注册 n 个用户、上传密钥、按小圈子互加好友、各发几条朋友圈、全部建立 WebSocket 并等积压推完。
func (w *world) setup(ctx context.Context, n int) {
	o := w.o
	w.users = make([]*user, n)
	t0 := time.Now()
	logf("注册 %d 个用户并上传密钥…", n)
	parallel(n, o.setupC, func(i int) {
		u := &user{idx: i, seen: map[uint64]struct{}{}, ready: make(chan struct{})}
		var res struct {
			UserID string `json:"user_id"`
			Token  string `json:"token"`
		}
		email := fmt.Sprintf("lt-%s-%05d@load.test", w.runID, i)
		w.must(ctx, "POST", "/v1/auth/verify", "", map[string]any{"email": email, "code": o.code, "device_name": "loadtest", "platform": "android"}, &res)
		u.id, u.token = res.UserID, res.Token
		otks := make([]map[string]string, 5)
		for j := range otks {
			otks[j] = map[string]string{"key_id": fmt.Sprintf("k%d", j), "key": fakeKey("otk", i, j), "signature": fakeKey("sig", i, j)}
		}
		w.must(ctx, "PUT", "/v1/keys", u.token, map[string]any{
			"identity_key": fakeKey("idk", i, 0), "signing_key": fakeKey("sgk", i, 0), "one_time_keys": otks,
			"fallback_key": map[string]string{"key_id": "fb", "key": fakeKey("fbk", i, 0), "signature": fakeKey("fbs", i, 0)},
		}, nil)
		w.users[i] = u
	})
	w.setupInfo.RegisterSec = time.Since(t0).Seconds()

	t1 := time.Now()
	circles := n / o.circle
	logf("建立 %d 个 %d 人小圈子（互为好友）并各发 %d 条朋友圈…", circles, o.circle, o.moments)
	parallel(circles, o.setupC, func(c int) {
		base := c * o.circle
		for a := 0; a < o.circle; a++ {
			for b := a + 1; b < o.circle; b++ {
				w.befriend(ctx, w.users[base+a], w.users[base+b])
			}
		}
		for a := 0; a < o.circle; a++ {
			u := w.users[base+a]
			for b := 0; b < o.circle; b++ {
				if a != b {
					u.friends = append(u.friends, base+b)
				}
			}
		}
	})
	// 不足一个圈子的尾巴并入最后一个圈子的第一个人
	for i := circles * o.circle; i < n; i++ {
		first := w.users[(circles-1)*o.circle]
		w.befriend(ctx, w.users[i], first)
		w.users[i].friends = append(w.users[i].friends, first.idx)
	}
	parallel(n, o.setupC, func(i int) {
		for k := 0; k < o.moments; k++ {
			w.must(ctx, "POST", "/v1/moments", w.users[i].token, map[string]any{"text": fmt.Sprintf("loadtest moment %d from %d", k, i)}, nil)
		}
	})
	w.setupInfo.FriendSec = time.Since(t1).Seconds()

	t2 := time.Now()
	logf("建立 %d 条 WebSocket…", n)
	parallel(n, o.setupC, func(i int) { w.connect(ctx, w.users[i]) })
	online := 0
	deadline := time.After(60 * time.Second)
	for _, u := range w.users {
		select {
		case <-u.ready:
			online++
		case <-deadline:
		}
	}
	w.setupInfo.ConnectSec = time.Since(t2).Seconds()
	w.setupInfo.Users, w.setupInfo.Circle, w.setupInfo.Online = n, o.circle, online
	if online != n {
		fatalf("只有 %d / %d 条连接收到 queue_empty", online, n)
	}
	go w.pingLoop()
	logf("就绪：%d 个用户在线（注册 %.1fs，好友 %.1fs，建连 %.1fs）", n, w.setupInfo.RegisterSec, w.setupInfo.FriendSec, w.setupInfo.ConnectSec)
}

// befriend 让 a 向 b 发好友申请、b 接受。已是好友（409）视为成功。
func (w *world) befriend(ctx context.Context, a, b *user) {
	var status int
	var raw []byte
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		status, raw, err = w.call(ctx, "POST", "/v1/friends/requests", a.token, map[string]any{"to_user_id": b.id, "message": "loadtest"})
		if err == nil && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
			time.Sleep(time.Duration(1+attempt) * 2 * time.Second)
			continue
		}
		break
	}
	if err != nil {
		fatalf("好友申请失败: %v", err)
	}
	if status == http.StatusConflict {
		return
	}
	if status/100 != 2 {
		fatalf("好友申请失败: HTTP %d %s", status, truncate(raw, 200))
	}
	var req struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		fatalf("解析好友申请失败: %v", err)
	}
	if req.Status == "accepted" {
		return
	}
	w.must(ctx, "POST", "/v1/friends/requests/"+req.ID+"/accept", b.token, nil, nil)
}

func (w *world) wsURL() string {
	return "ws" + strings.TrimPrefix(w.o.base, "http") + "/v1/ws"
}

func (w *world) connect(ctx context.Context, u *user) {
	var c *websocket.Conn
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		c, _, err = websocket.Dial(dctx, w.wsURL(), &websocket.DialOptions{
			HTTPClient: w.hc,
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + u.token}},
		})
		cancel()
		if err == nil {
			break
		}
		time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		fatalf("WebSocket 连接失败 user=%d: %v", u.idx, err)
	}
	c.SetReadLimit(4 << 20)
	u.ws = c
	go w.readLoop(u)
}

var (
	envPrefix  = []byte(`{"t":"envelope"`)
	idMarker   = []byte(`"id":"`)
	bodyMarker = []byte(`"body":"L`)
	emptyFrame = []byte(`{"t":"queue_empty"}`)
)

// readLoop 读取服务端帧：信封解析出发送时刻记录延迟，信封与事件都 ack（与真实客户端一致，默认逐条）。
func (w *world) readLoop(u *user) {
	ctx := context.Background()
	readyOnce := false
	for {
		_, data, err := u.ws.Read(ctx)
		if err != nil {
			if !w.closed.Load() {
				w.wsErrors.Add(1)
				logf("WebSocket 断开 user=%d: %v", u.idx, err)
			}
			return
		}
		now := time.Now().UnixNano()
		switch {
		case bytes.HasPrefix(data, envPrefix):
			id := between(data, idMarker)
			w.record(u, data, now)
			w.ack(ctx, u, id)
		case bytes.Equal(data, emptyFrame):
			if !readyOnce {
				readyOnce = true
				close(u.ready)
			}
		case bytes.HasPrefix(data, []byte(`{"t":"event"`)):
			var f struct {
				D struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"d"`
			}
			if json.Unmarshal(data, &f) == nil {
				u.events.Add(1)
				if f.D.Type == "group_updated" {
					u.groupEvent.Add(1)
				}
				if f.D.ID != "" {
					w.ack(ctx, u, f.D.ID)
				}
			}
		}
	}
}

// between 返回 marker 之后到下一个引号之前的内容。
func between(data, marker []byte) string {
	i := bytes.Index(data, marker)
	if i < 0 {
		return ""
	}
	rest := data[i+len(marker):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

// record 从信封 body（"L<阶段>.<发送纳秒>.<序号>." + 填充）里取出发送时刻，记录端到端延迟。
func (w *world) record(u *user, frame []byte, now int64) {
	i := bytes.Index(frame, bodyMarker)
	if i < 0 {
		return
	}
	rest := frame[i+len(bodyMarker):]
	if len(rest) > 80 {
		rest = rest[:80]
	}
	parts := strings.SplitN(string(rest), ".", 4)
	if len(parts) < 4 {
		return
	}
	phase, err1 := strconv.ParseInt(parts[0], 10, 64)
	sent, err2 := strconv.ParseInt(parts[1], 10, 64)
	seq, err3 := strconv.ParseUint(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	u.mu.Lock()
	if phase == u.phase {
		if _, dup := u.seen[seq]; dup {
			u.dups++
		} else {
			u.seen[seq] = struct{}{}
			u.lat = append(u.lat, now-sent)
		}
	}
	u.mu.Unlock()
}

func (w *world) ack(ctx context.Context, u *user, id string) {
	if id == "" {
		return
	}
	u.pending = append(u.pending, id) // 只有 readLoop 协程访问
	if len(u.pending) < w.o.ackBatch {
		return
	}
	w.flushAck(ctx, u)
}

func (w *world) flushAck(ctx context.Context, u *user) {
	if len(u.pending) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(`{"t":"ack","ids":[`)
	for i, id := range u.pending {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(id)
		b.WriteByte('"')
	}
	b.WriteString(`]}`)
	u.pending = u.pending[:0]
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := u.ws.Write(wctx, websocket.MessageText, []byte(b.String())); err != nil && !w.closed.Load() {
		w.wsErrors.Add(1)
	}
}

// pingLoop 每 25 秒给全部连接发一次 ping（与真实客户端一致；服务端 90 秒收不到任何帧会断开）。
func (w *world) pingLoop() {
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	ping := []byte(`{"t":"ping"}`)
	for range tick.C {
		if w.closed.Load() {
			return
		}
		for _, u := range w.users {
			if u.ws == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			u.ws.Write(ctx, websocket.MessageText, ping)
			cancel()
		}
	}
}

func (w *world) closeAll() {
	w.closed.Store(true)
	parallel(len(w.users), 64, func(i int) {
		if u := w.users[i]; u != nil && u.ws != nil {
			u.ws.Close(websocket.StatusNormalClosure, "done")
		}
	})
}

// beginPhase 开始一个新的测量阶段：清空各用户的延迟样本与判重表。
func (w *world) beginPhase() int64 {
	p := w.phase.Add(1)
	for _, u := range w.users {
		u.mu.Lock()
		u.phase = p
		u.lat = u.lat[:0]
		u.seen = map[uint64]struct{}{}
		u.dups = 0
		u.mu.Unlock()
	}
	return p
}

// collect 汇总当前阶段全部接收方的延迟样本、收到条数与重复条数。
func (w *world) collect() (lat []int64, dups int64) {
	for _, u := range w.users {
		u.mu.Lock()
		lat = append(lat, u.lat...)
		dups += u.dups
		u.mu.Unlock()
	}
	return lat, dups
}

// received 返回当前阶段已收到（去重后）的信封总数。
func (w *world) received() int64 {
	var n int64
	for _, u := range w.users {
		u.mu.Lock()
		n += int64(len(u.lat))
		u.mu.Unlock()
	}
	return n
}

// settle 等到收到的条数达到 expected，或者超时。
func (w *world) settle(expected int64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if w.received() >= expected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// body 生成带发送时刻的消息体：L<阶段>.<纳秒>.<序号>. 后面用 x 填充到 size 字节。
func makeBody(phase int64, seq uint64, size int, pad []byte) string {
	head := "L" + strconv.FormatInt(phase, 10) + "." + strconv.FormatInt(time.Now().UnixNano(), 10) + "." + strconv.FormatUint(seq, 10) + "."
	if len(head) >= size {
		return head
	}
	return head + string(pad[:size-len(head)])
}

type group struct {
	id      string
	members []*user
}

// makeGroup 用前 n 个用户建一个群：其余成员先各自向群主发好友申请、群主接受（建群要求成员都是群主好友）。
func (w *world) makeGroup(ctx context.Context, n int) *group {
	if n > len(w.users) {
		fatalf("群人数 %d 超过用户数 %d", n, len(w.users))
	}
	owner := w.users[0]
	logf("建 %d 人群（成员先加群主为好友）…", n)
	parallel(n-1, w.o.setupC, func(i int) { w.befriend(ctx, w.users[i+1], owner) })
	ids := make([]string, 0, n-1)
	var before int64
	for _, u := range w.users[:n] {
		before += u.groupEvent.Load()
	}
	for _, u := range w.users[1:n] {
		ids = append(ids, u.id)
	}
	var g struct {
		ID string `json:"id"`
	}
	w.must(ctx, "POST", "/v1/groups", owner.token, map[string]any{"name": fmt.Sprintf("loadtest-%d", n), "member_ids": ids}, &g)
	// 等全体成员收到 group_updated
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var got int64
		for _, u := range w.users[:n] {
			got += u.groupEvent.Load()
		}
		if got-before >= int64(n) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	return &group{id: g.ID, members: w.users[:n]}
}
