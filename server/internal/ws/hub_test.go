package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hush/server/internal/push"
	"hush/server/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("HUSH_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}

type testEnv struct {
	st  *store.Store
	hub *Hub
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &testEnv{st: st, hub: NewHub(st, push.LogPusher{})}
}

func (e *testEnv) login(t *testing.T, email, tokenHash string) string {
	t.Helper()
	res, err := e.st.Login(context.Background(), email, "android", "t", tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	return res.User.ID
}

func (e *testEnv) queueIDs(t *testing.T, uid string) []string {
	t.Helper()
	items, err := e.st.QueueAfter(context.Background(), uid, 0, 10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// ---------- 单设备登录与 WS 的时序（审查 S2） ----------

func (e *testEnv) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hub.Serve(w, r, r.URL.Query().Get("uid"), r.URL.Query().Get("th"))
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

func dialWS(t *testing.T, srv *httptest.Server, uid, th string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/?uid="+uid+"&th="+th, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetReadLimit(1 << 22)
	t.Cleanup(func() { c.CloseNow() })
	return c
}

type testFrame struct {
	T string          `json:"t"`
	D json.RawMessage `json:"d"`
}

// readFrame 读下一帧；连接关闭时返回关闭码。
func readFrame(t *testing.T, c *websocket.Conn) (testFrame, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		code := websocket.CloseStatus(err)
		if code == -1 {
			t.Fatalf("读取失败: %v", err)
		}
		return testFrame{}, code
	}
	var f testFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f, 0
}

func readUntil(t *testing.T, c *websocket.Conn, typ string) testFrame {
	t.Helper()
	for {
		f, code := readFrame(t, c)
		if code != 0 {
			t.Fatalf("等待 %s 时连接被关闭: %d", typ, code)
		}
		if f.T == typ {
			return f
		}
	}
}

func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

func envelopeItem(uid, id string) Item {
	return Item{UserID: uid, ID: id, Kind: store.KindEnvelope, Payload: []byte(`{"id":"` + id + `"}`)}
}

// 旧设备在重新登录生效之前通过了鉴权、在 Kick 之后才登记：必须以 4001 被拒，且不能以 4000 挤掉新设备。
func TestStaleConnectionAfterReloginRejected(t *testing.T) {
	e := newTestEnv(t)
	uid := e.login(t, "a@example.com", "h1")
	srv := e.server(t)

	var fired atomic.Bool
	var newConn *websocket.Conn
	e.hub.beforeRegister = func(string) {
		if fired.Swap(true) {
			return // 只对第一个连接（旧设备）插入重新登录
		}
		// 旧连接鉴权之后、登记之前：新设备登录（作废旧 token）→ Kick → 新设备连上
		if _, err := e.st.Login(context.Background(), "a@example.com", "ios", "new", "h2"); err != nil {
			t.Error(err)
		}
		e.hub.Kick(uid, map[string]any{"t": "event"}, CloseLoggedOut, "logged_in_elsewhere")
		newConn = dialWS(t, srv, uid, "h2")
		readUntil(t, newConn, "queue_empty")
	}

	stale := dialWS(t, srv, uid, "h1")
	for {
		_, code := readFrame(t, stale)
		if code != 0 {
			if code != CloseLoggedOut {
				t.Fatalf("旧连接应以 4001 关闭，得到 %d", code)
			}
			break
		}
	}
	// 新设备仍是当前连接，照常收到新数据
	if _, err := e.hub.Deliver(context.Background(), []Item{envelopeItem(uid, "m1")}, store.QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	f, code := readFrame(t, newConn)
	if code != 0 || f.T != "envelope" {
		t.Fatalf("新设备应收到信封（没有被旧连接挤掉）: %+v code=%d", f, code)
	}
}

// 重新登录已提交、旧连接尚未被踢时：旧连接收到发给新设备的信封，但它的 ack 不能删掉这些数据。
func TestAckWithRevokedTokenIgnored(t *testing.T) {
	e := newTestEnv(t)
	uid := e.login(t, "a@example.com", "h1")
	srv := e.server(t)
	old := dialWS(t, srv, uid, "h1")
	readUntil(t, old, "queue_empty")

	if _, err := e.st.Login(context.Background(), "a@example.com", "ios", "new", "h2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hub.Deliver(context.Background(), []Item{envelopeItem(uid, "for-new-device")}, store.QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	readUntil(t, old, "envelope")
	sendJSON(t, old, map[string]any{"t": "ack", "ids": []string{"for-new-device"}})
	for {
		_, code := readFrame(t, old)
		if code != 0 {
			if code != CloseLoggedOut {
				t.Fatalf("token 已作废的连接 ack 后应以 4001 关闭，得到 %d", code)
			}
			break
		}
	}
	if ids := e.queueIDs(t, uid); len(ids) != 1 || ids[0] != "for-new-device" {
		t.Fatalf("发给新设备的信封不应被旧设备 ack 删除: %v", ids)
	}
}

// WS 的 ack 超过 1000 个 id 时分批全部确认，而不是静默截断（审查 S14）。
func TestWSAckMoreThanMax(t *testing.T) {
	e := newTestEnv(t)
	uid := e.login(t, "a@example.com", "h1")
	const n = 2500
	var items []Item
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m%04d", i)
		items = append(items, envelopeItem(uid, id))
		ids = append(ids, id)
	}
	if _, err := e.hub.Deliver(context.Background(), items, store.QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	srv := e.server(t)
	c := dialWS(t, srv, uid, "h1")
	readUntil(t, c, "queue_empty")
	sendJSON(t, c, map[string]any{"t": "ack", "ids": ids})
	sendJSON(t, c, map[string]any{"t": "ping"})
	readUntil(t, c, "pong") // ack 在 ping 之前处理完
	if left := e.queueIDs(t, uid); len(left) != 0 {
		t.Fatalf("应全部确认，剩余 %d 条", len(left))
	}
}
