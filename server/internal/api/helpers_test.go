package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hush/server/internal/api"
	"hush/server/internal/store"
	"hush/server/internal/ws"
)

const devCode = "424242"
const testTurnSecret = "test-turn-secret"

func TestMain(m *testing.M) {
	if os.Getenv("HUSH_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}

// captureMailer 记录最近发出的验证码。
type captureMailer struct {
	mu    sync.Mutex
	codes map[string]string
}

func (m *captureMailer) SendCode(to, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[to] = code
	return nil
}

func (m *captureMailer) code(email string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.codes[email]
}

// recordPusher 记录离线推送调用。
type recordPusher struct {
	mu    sync.Mutex
	users []string
	flush func() // 离线推送是异步的：计数前先等排队中的推送处理完
}

func (p *recordPusher) Push(userID, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, userID)
}

func (p *recordPusher) count(uid string) int {
	if p.flush != nil {
		p.flush()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, u := range p.users {
		if u == uid {
			n++
		}
	}
	return n
}

type env struct {
	t       *testing.T
	dataDir string
	srv     *httptest.Server
	st      *store.Store
	mailer  *captureMailer
	pusher  *recordPusher
	hub     *ws.Hub
}

// wsDial 直接拨号（不经 client.dial 的失败即终止）。
func wsDial(ctx context.Context, url string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, url, nil)
}

var emailSeq atomic.Int64

// newEnv 启动测试服务。opts 可修改配置；若设置了 timeouts，则用带全局超时的 http.Server。
func newEnv(t *testing.T, withDevCode bool, opts ...func(*api.Config)) *env {
	return newEnvTimeouts(t, withDevCode, nil, opts...)
}

func newEnvTimeouts(t *testing.T, withDevCode bool, timeouts *api.Timeouts, opts ...func(*api.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, mailer: &captureMailer{codes: map[string]string{}}, pusher: &recordPusher{}}
	hub := ws.NewHub(st, e.pusher)
	e.hub = hub
	e.pusher.flush = hub.FlushPushes
	cfg := api.Config{
		Store: st, Hub: hub, Mailer: e.mailer, DataDir: dir,
		TurnSecret: testTurnSecret, TurnPort: 3478, TurnTTL: time.Hour,
		MaxAttachmentBytes: 1 << 20,
		// 测试里所有请求都来自 127.0.0.1、同一用户会反复操作：限速默认放开，专门的用例再调低
		WritesPerMinute: 1 << 20, AuthCodePerIPPerMinute: 1 << 20, AuthVerifyPerIPPerMinute: 1 << 20,
	}
	if withDevCode {
		cfg.DevCode = devCode
	}
	for _, o := range opts {
		o(&cfg)
	}
	e.dataDir = dir
	e.srv = httptest.NewUnstartedServer(api.New(cfg))
	if timeouts != nil {
		api.ApplyTimeouts(e.srv.Config, *timeouts)
	}
	e.srv.Start()
	t.Cleanup(func() {
		e.srv.CloseClientConnections()
		e.srv.Close()
		st.Close()
	})
	return e
}

// do 发起请求并返回状态码与响应体。body 为 []byte 时原样发送，否则编码为 JSON。
func (e *env) do(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

type client struct {
	e     *env
	ID    string
	Token string
	Email string
}

// register 用开发验证码注册/登录一个新用户。
func (e *env) register(name string) *client {
	e.t.Helper()
	email := fmt.Sprintf("%s%d@example.com", name, emailSeq.Add(1))
	return e.login(email)
}

func (e *env) login(email string) *client {
	e.t.Helper()
	var res struct {
		UserID string `json:"user_id"`
		Token  string `json:"token"`
	}
	status, body := e.do("POST", "/v1/auth/verify", "", map[string]any{"email": email, "code": devCode, "device_name": "test", "platform": "android"})
	if status != 200 {
		e.t.Fatalf("login %s: %d %s", email, status, body)
	}
	json.Unmarshal(body, &res)
	return &client{e: e, ID: res.UserID, Token: res.Token, Email: email}
}

// call 发起已认证请求，out 非 nil 时解码响应体。
func (c *client) call(method, path string, body any, out any) (int, []byte) {
	c.e.t.Helper()
	status, data := c.e.do(method, path, c.Token, body)
	if out != nil && len(data) > 0 {
		v := reflect.ValueOf(out).Elem()
		v.Set(reflect.Zero(v.Type()))
		if err := json.Unmarshal(data, out); err != nil {
			c.e.t.Fatalf("%s %s: 解码响应失败: %v (%s)", method, path, err, data)
		}
	}
	return status, data
}

// must 发起请求并要求 200。
func (c *client) must(method, path string, body any, out any) {
	c.e.t.Helper()
	status, data := c.call(method, path, body, out)
	if status != 200 {
		c.e.t.Fatalf("%s %s: 期望 200，得到 %d %s", method, path, status, data)
	}
}

// expectErr 要求返回指定 HTTP 状态与错误码。
func (c *client) expectErr(method, path string, body any, status int, code string) {
	c.e.t.Helper()
	got, data := c.call(method, path, body, nil)
	var eb struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	json.Unmarshal(data, &eb)
	if got != status || eb.Error != code || eb.Message == "" {
		c.e.t.Fatalf("%s %s: 期望 %d/%s，得到 %d %s", method, path, status, code, got, data)
	}
}

// befriend 让 a、b 成为好友。
func befriend(t *testing.T, a, b *client) {
	t.Helper()
	var fr struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID, "message": "hi"}, &fr)
	b.must("POST", "/v1/friends/requests/"+fr.ID+"/accept", nil, nil)
}

// ---------- WebSocket 测试客户端 ----------

type frame struct {
	T string          `json:"t"`
	D json.RawMessage `json:"d"`
}

func (f frame) event() map[string]any {
	var m map[string]any
	json.Unmarshal(f.D, &m)
	return m
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func (c *client) dial() *wsClient {
	c.e.t.Helper()
	url := "ws" + strings.TrimPrefix(c.e.srv.URL, "http") + "/v1/ws?token=" + c.Token
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		c.e.t.Fatalf("ws dial: %v", err)
	}
	conn.SetReadLimit(1 << 22)
	w := &wsClient{t: c.e.t, conn: conn}
	c.e.t.Cleanup(func() { conn.CloseNow() })
	return w
}

// read 读取下一帧，超时失败。
func (w *wsClient) read() frame {
	w.t.Helper()
	f, err := w.tryRead(3 * time.Second)
	if err != nil {
		w.t.Fatalf("ws read: %v", err)
	}
	return f
}

func (w *wsClient) tryRead(timeout time.Duration) (frame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := w.conn.Read(ctx)
	if err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		w.t.Fatalf("ws 帧不是 JSON: %s", data)
	}
	return f, nil
}

// until 读取直到出现满足条件的帧（跳过其他帧）。
func (w *wsClient) until(desc string, pred func(frame) bool) frame {
	w.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f, err := w.tryRead(time.Until(deadline))
		if err != nil {
			w.t.Fatalf("等待 %s 失败: %v", desc, err)
		}
		if pred(f) {
			return f
		}
	}
	w.t.Fatalf("等待 %s 超时", desc)
	return frame{}
}

// event 等待指定类型事件并返回其 d。
func (w *wsClient) event(typ string) map[string]any {
	w.t.Helper()
	f := w.until("事件 "+typ, func(f frame) bool { return f.T == "event" && f.event()["type"] == typ })
	return f.event()
}

func (w *wsClient) send(v any) {
	w.t.Helper()
	raw, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		w.t.Fatalf("ws write: %v", err)
	}
}

// expectSilence 断言短时间内没有帧到达。注意：coder/websocket 读超时会关闭连接，只能作为该连接的最后一步。
func (w *wsClient) expectSilence(d time.Duration) {
	w.t.Helper()
	if f, err := w.tryRead(d); err == nil {
		w.t.Fatalf("期望无帧，却收到 %s %s", f.T, f.D)
	}
}

// envelope 是测试用信封结构。
type envelope struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	From      string  `json:"from"`
	Type      int     `json:"type"`
	GroupID   *string `json:"group_id"`
	SessionID *string `json:"session_id"`
	Body      string  `json:"body"`
	ServerTS  int64   `json:"server_ts"`
}

func (f frame) envelope() envelope {
	var e envelope
	json.Unmarshal(f.D, &e)
	return e
}
