package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hush/server/internal/store"
)

type countPusher struct {
	mu    sync.Mutex
	users []string
	block chan struct{}
}

func (p *countPusher) Push(userID, _ string) {
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	p.users = append(p.users, userID)
	p.mu.Unlock()
}

func (p *countPusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.users)
}

func env(uid string, i int) Item {
	id := fmt.Sprint("m", i)
	return Item{UserID: uid, ID: id, Kind: store.KindEnvelope, Payload: []byte(`{"id":"` + id + `","n":` + fmt.Sprint(i) + `}`), Urgent: true}
}

// 提交钩子直投：大量消息并发入队（远超连接的实时缓冲，必然触发「溢出 → 回库补读」），
// 在线连接收到的仍然是完整的、严格按 seq 递增的、不重复的序列。
func TestLiveHandoffOrderedUnderOverflow(t *testing.T) {
	e := newTestEnv(t)
	uid := e.login(t, "a@example.com", "ha")
	srv := e.server(t)
	c := dialWS(t, srv, uid, "ha")
	readUntil(t, c, "queue_empty")

	const total = 3000 // liveMaxItems 的十几倍
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < total; i += 8 {
				if _, err := e.hub.Deliver(context.Background(), []Item{env(uid, i)}, store.QueueLimits{}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	// 数据库里的顺序就是期望的投递顺序
	seen := map[string]bool{}
	var got []string
	for len(got) < total {
		f, code := readFrame(t, c)
		if code != 0 {
			t.Fatalf("连接被关闭: %d（已收 %d）", code, len(got))
		}
		if f.T != "envelope" {
			continue
		}
		var d struct {
			ID string `json:"id"`
		}
		json.Unmarshal(f.D, &d)
		if seen[d.ID] {
			t.Fatalf("同一连接上重复投递 %s", d.ID)
		}
		seen[d.ID] = true
		got = append(got, d.ID)
	}
	wg.Wait()
	want := e.queueIDs(t, uid)
	if len(want) != total {
		t.Fatalf("队列里应有 %d 条，得到 %d", total, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条顺序不对：收到 %s，队列里是 %s", i, got[i], want[i])
		}
	}
}

// A10：Shutdown 给在线连接发 1001 关闭帧并等连接协程退出；之后关库是安全的，未 ack 的数据仍在队列里。
func TestShutdownClosesConnectionsGracefully(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{st: st, hub: NewHub(st, nil)}
	uid := e.login(t, "a@example.com", "ha")
	srv := e.server(t)
	c := dialWS(t, srv, uid, "ha")
	readUntil(t, c, "queue_empty")
	if _, err := e.hub.Deliver(context.Background(), []Item{env(uid, 1)}, store.QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "envelope")

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- e.hub.Shutdown(ctx)
	}()
	_, code := readFrame(t, c)
	if code != websocket.StatusGoingAway {
		t.Fatalf("停机时应收到关闭码 1001，得到 %d", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown 应在连接关闭后返回: %v", err)
	}
	if e.hub.OnlineCount() != 0 {
		t.Fatalf("停机后不应还有在线连接: %d", e.hub.OnlineCount())
	}
	// 停机之后不再接受新连接
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/?uid="+uid+"&th=ha", nil); err == nil {
		conn.CloseNow()
		t.Fatal("停机后不应再接受新连接")
	}
	if err := e.hub.Acquire("198.51.100.1"); err != ErrShuttingDown {
		t.Fatalf("停机后 Acquire 应返回 ErrShuttingDown，得到 %v", err)
	}
	if ids := e.queueIDs(t, uid); len(ids) != 1 {
		t.Fatalf("未 ack 的数据应仍在队列里: %v", ids)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("连接都退出之后关库: %v", err)
	}
}

// B6：重新登录的事务提交之后，旧连接立刻被踢（提交回调里），此后入队的信封不会再交给旧设备。
func TestReloginKicksOldConnectionBeforeLaterDeliveries(t *testing.T) {
	e := newTestEnv(t)
	uid := e.login(t, "a@example.com", "h-old")
	srv := e.server(t)
	old := dialWS(t, srv, uid, "h-old")
	readUntil(t, old, "queue_empty")

	// 重新登录与「给该用户发消息」并发进行很多轮：旧连接上绝不能出现重新登录之后才入队的信封
	err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Login("a@example.com", "ios", "new", "h-new"); err != nil {
			return err
		}
		tx.OnCommit(func() {
			e.hub.Kick(uid, map[string]any{"t": "event", "d": map[string]any{"type": "logged_out"}}, CloseLoggedOut, "logged_in_elsewhere")
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Update 返回时提交回调已经执行：旧连接已不是当前连接
	if e.hub.IsOnline(uid) {
		t.Fatal("重新登录提交之后旧连接应已被摘除")
	}
	if _, err := e.hub.Deliver(context.Background(), []Item{{UserID: uid, ID: "for-new-device", Kind: store.KindEnvelope, Payload: []byte(`{"id":"for-new-device"}`)}}, store.QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	for {
		f, code := readFrame(t, old)
		if code != 0 {
			if code != CloseLoggedOut {
				t.Fatalf("旧连接应以 4001 关闭，得到 %d", code)
			}
			break
		}
		if f.T == "envelope" {
			t.Fatalf("旧设备不应收到重新登录之后的信封: %s", f.D)
		}
	}
}

// B7：Push 在独立协程里执行——推送服务卡住时，发送（提交钩子）不被拖住。
func TestPushIsAsync(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := &countPusher{block: make(chan struct{})}
	e := &testEnv{st: st, hub: NewHub(st, p)}
	uid := e.login(t, "a@example.com", "ha")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			e.hub.Deliver(context.Background(), []Item{env(uid, i)}, store.QueueLimits{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("推送服务卡住时发送不应被阻塞")
	}
	close(p.block)
	e.hub.FlushPushes()
	if p.count() != 20 {
		t.Fatalf("离线用户的 urgent 信封应各触发一次推送，得到 %d", p.count())
	}
}

// B7：连接很久没有收到客户端的任何帧（可能是半开连接）时，urgent 信封在照常下发的同时也触发离线推送。
func TestStaleConnectionAlsoPushes(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := &countPusher{}
	e := &testEnv{st: st, hub: NewHub(st, p)}
	uid := e.login(t, "a@example.com", "ha")
	srv := e.server(t)
	c := dialWS(t, srv, uid, "ha")
	readUntil(t, c, "queue_empty")
	e.hub.Deliver(context.Background(), []Item{env(uid, 1)}, store.QueueLimits{})
	readUntil(t, c, "envelope")
	e.hub.FlushPushes()
	if p.count() != 0 {
		t.Fatal("连接活跃时不应触发离线推送")
	}
	e.hub.get(uid).lastRead.Store(time.Now().Add(-2 * staleAfter).UnixNano())
	e.hub.Deliver(context.Background(), []Item{env(uid, 2)}, store.QueueLimits{})
	readUntil(t, c, "envelope") // 照常下发
	e.hub.FlushPushes()
	if p.count() != 1 {
		t.Fatalf("疑似半开的连接上，urgent 信封应同时触发推送，得到 %d", p.count())
	}
}

// B5：epoch 表不随被踢过的用户数无限增长。
func TestEpochsPruned(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < 50; i++ {
		e.hub.Disconnect(fmt.Sprint("user-", i), CloseLoggedOut, "x")
	}
	if n := e.hub.PruneEpochs(time.Now()); n != 0 {
		t.Fatalf("刚产生的 epoch 不应被清理: %d", n)
	}
	if n := e.hub.PruneEpochs(time.Now().Add(2 * epochTTL)); n != 50 {
		t.Fatalf("过期的 epoch 应全部清理，得到 %d", n)
	}
	if e.hub.currentEpoch("user-1") != 0 {
		t.Fatal("清理后应回到初始值")
	}
}

// 连接数上限。
func TestAcquireLimits(t *testing.T) {
	e := newTestEnv(t)
	e.hub.SetLimits(3, 2)
	for i, c := range []struct {
		ip   string
		want error
	}{
		{"10.0.0.1", nil}, {"10.0.0.1", nil}, {"10.0.0.1", ErrTooManyConnsPerIP}, {"10.0.0.2", nil}, {"10.0.0.3", ErrTooManyConns},
	} {
		if err := e.hub.Acquire(c.ip); err != c.want {
			t.Fatalf("第 %d 次 Acquire(%s) = %v，期望 %v", i, c.ip, err, c.want)
		}
	}
	e.hub.Release("10.0.0.1")
	if err := e.hub.Acquire("10.0.0.3"); err != nil {
		t.Fatalf("释放后应可再连: %v", err)
	}
}

// 同一批帧合并写出：corkConn 在「塞住」期间攒小帧，拔掉时一次写出；大帧直接写。
func TestCorkConnCoalescesWrites(t *testing.T) {
	rec := &recordConn{}
	c := &corkConn{Conn: rec}
	c.setCork(true)
	for i := 0; i < 10; i++ {
		c.Write([]byte("frame"))
	}
	if rec.writes != 0 {
		t.Fatalf("塞住期间不应有底层写: %d", rec.writes)
	}
	c.setCork(false)
	if rec.writes != 1 || rec.bytes != 50 {
		t.Fatalf("拔掉塞子应一次写出全部 50 字节: %d 次 / %d 字节", rec.writes, rec.bytes)
	}
	c.setCork(true)
	c.Write([]byte("small"))
	c.Write(make([]byte, corkBigWrite)) // 大帧：先刷出攒着的，再直接写
	if rec.writes != 3 {
		t.Fatalf("大帧应直接写出（先刷出之前攒的）: %d", rec.writes)
	}
	c.setCork(false)
	c.Write([]byte("x"))
	if rec.writes != 4 {
		t.Fatalf("没塞住时直接写: %d", rec.writes)
	}
}

type recordConn struct {
	net.Conn
	writes, bytes int
}

func (r *recordConn) Write(p []byte) (int, error) {
	r.writes++
	r.bytes += len(p)
	return len(p), nil
}
