package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"

	"hush/server/internal/admin"
	"hush/server/internal/api"
	"hush/server/internal/store"
	"hush/server/internal/ws"
)

const devCode = "424242"
const testPassword = "correct-horse-battery"

func TestMain(m *testing.M) {
	if os.Getenv("HUSH_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}

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

// clock 是可拨动的时钟（会话过期、登录锁定的测试用）。
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

type env struct {
	rawDB   *sql.DB
	t       *testing.T
	dataDir string
	st      *store.Store
	hub     *ws.Hub
	api     *api.Server
	adm     *admin.Server
	apiSrv  *httptest.Server
	admSrv  *httptest.Server
	mailer  *captureMailer
	clock   *clock
}

var seq atomic.Int64

func newEnv(t *testing.T, opts ...func(*api.Config, *admin.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dataDir: dir, st: st, mailer: &captureMailer{codes: map[string]string{}}, clock: &clock{}}
	e.hub = ws.NewHub(st, nil)
	acfg := api.Config{Store: st, Hub: e.hub, Mailer: e.mailer, DataDir: dir, DevCode: devCode, MaxAttachmentBytes: 1 << 20,
		WritesPerMinute: 1 << 20, AuthCodePerIPPerMinute: 1 << 20, AuthVerifyPerIPPerMinute: 1 << 20, ReportsPerHour: 1 << 20}
	mcfg := admin.Config{Store: st, Hub: e.hub, DataDir: dir, Version: "test", BcryptCost: bcrypt.MinCost,
		LoginPerIPPerMinute: 1 << 20, Now: e.clock.now}
	for _, o := range opts {
		o(&acfg, &mcfg)
	}
	e.api = api.NewServer(acfg)
	mcfg.API = e.api
	e.adm = admin.New(mcfg)
	e.apiSrv = httptest.NewServer(e.api.Handler())
	e.admSrv = httptest.NewServer(e.adm.Handler())
	t.Cleanup(func() {
		e.apiSrv.CloseClientConnections()
		e.apiSrv.Close()
		e.admSrv.Close()
		st.Close()
	})
	return e
}

// ---------- 管理员会话 ----------

type session struct {
	e      *env
	client *http.Client
	csrf   string
	admin  store.Admin
}

// mkAdmin 直接在库里建一个管理员（低成本 bcrypt）并登录。
func (e *env) mkAdmin(username, role string) *session {
	e.t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if _, err := e.st.CreateAdmin(context.Background(), username, string(hash), role); err != nil {
		e.t.Fatal(err)
	}
	s, status, body := e.loginAs(username, testPassword)
	if status != 200 {
		e.t.Fatalf("管理员登录失败: %d %s", status, body)
	}
	return s
}

func (e *env) loginAs(username, password string) (*session, int, []byte) {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	s := &session{e: e, client: &http.Client{Jar: jar}}
	status, body, _ := s.raw("POST", "/admin/api/login", map[string]any{"username": username, "password": password}, nil)
	var res struct {
		Admin store.Admin `json:"admin"`
		CSRF  string      `json:"csrf"`
	}
	json.Unmarshal(body, &res)
	s.csrf, s.admin = res.CSRF, res.Admin
	return s, status, body
}

// raw 发起请求；hdr 可覆盖 / 删除请求头（值为空串表示不带）。
func (s *session) raw(method, path string, body any, hdr map[string]string) (int, []byte, http.Header) {
	s.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.e.admSrv.URL+path, rd)
	if err != nil {
		s.e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.csrf != "" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func (s *session) do(method, path string, body any) (int, []byte) {
	status, data, _ := s.raw(method, path, body, nil)
	return status, data
}

// must 要求 200 并把响应解码到 out。
func (s *session) must(method, path string, body, out any) {
	s.e.t.Helper()
	status, data := s.do(method, path, body)
	if status != 200 {
		s.e.t.Fatalf("%s %s: 期望 200，得到 %d %s", method, path, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			s.e.t.Fatalf("%s %s: 解码失败 %v (%s)", method, path, err, data)
		}
	}
}

// expect 要求指定状态码与错误码。
func (s *session) expect(method, path string, body any, status int, code string) {
	s.e.t.Helper()
	got, data := s.do(method, path, body)
	var eb struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &eb)
	if got != status || eb.Error != code {
		s.e.t.Fatalf("%s %s: 期望 %d/%s，得到 %d %s", method, path, status, code, got, data)
	}
}

// audited 断言审计日志里有 action（且 target 含 targetPart）。
func (e *env) audited(action, targetPart string) store.AuditEntry {
	e.t.Helper()
	list, _, err := e.st.ListAudit(context.Background(), store.AuditFilter{Action: action}, 1, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range list {
		if a.Action == action && strings.Contains(a.Target, targetPart) {
			if a.AdminName == "" || a.TS == 0 || a.IP == "" {
				e.t.Fatalf("审计记录不完整: %+v", a)
			}
			return a
		}
	}
	e.t.Fatalf("审计日志里没有 %s（对象含 %q）", action, targetPart)
	return store.AuditEntry{}
}

// ---------- 普通用户（走客户端接口） ----------

type user struct {
	e     *env
	ID    string
	Token string
	Email string
}

func (e *env) apiDo(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.apiSrv.URL+path, rd)
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

// verify 调用 /v1/auth/verify（开发验证码），返回状态码、错误码与用户。
func (e *env) verify(email, invite string) (int, string, *user) {
	e.t.Helper()
	body := map[string]any{"email": email, "code": devCode, "device_name": "test", "platform": "android"}
	if invite != "" {
		body["invite_code"] = invite
	}
	status, data := e.apiDo("POST", "/v1/auth/verify", "", body)
	var res struct {
		UserID string `json:"user_id"`
		Token  string `json:"token"`
		Error  string `json:"error"`
	}
	json.Unmarshal(data, &res)
	return status, res.Error, &user{e: e, ID: res.UserID, Token: res.Token, Email: email}
}

func (e *env) register(name string) *user {
	e.t.Helper()
	email := fmt.Sprintf("%s%d@example.com", name, seq.Add(1))
	status, code, u := e.verify(email, "")
	if status != 200 {
		e.t.Fatalf("注册 %s 失败: %d %s", email, status, code)
	}
	return u
}

func (u *user) call(method, path string, body, out any) (int, []byte) {
	u.e.t.Helper()
	status, data := u.e.apiDo(method, path, u.Token, body)
	if out != nil && status == 200 {
		if err := json.Unmarshal(data, out); err != nil {
			u.e.t.Fatalf("%s %s: 解码失败 %v (%s)", method, path, err, data)
		}
	}
	return status, data
}

func (u *user) must(method, path string, body, out any) {
	u.e.t.Helper()
	if status, data := u.call(method, path, body, out); status != 200 {
		u.e.t.Fatalf("%s %s: 期望 200，得到 %d %s", method, path, status, data)
	}
}

func (u *user) expect(method, path string, body any, status int, code string) {
	u.e.t.Helper()
	got, data := u.call(method, path, body, nil)
	var eb struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &eb)
	if got != status || eb.Error != code {
		u.e.t.Fatalf("%s %s: 期望 %d/%s，得到 %d %s", method, path, status, code, got, data)
	}
}

func befriend(t *testing.T, a, b *user) {
	t.Helper()
	var fr struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, &fr)
	b.must("POST", "/v1/friends/requests/"+fr.ID+"/accept", nil, nil)
}

func (u *user) createGroup(name string, members ...*user) string {
	u.e.t.Helper()
	ids := []string{}
	for _, m := range members {
		ids = append(ids, m.ID)
	}
	var g struct {
		ID string `json:"id"`
	}
	u.must("POST", "/v1/groups", map[string]any{"name": name, "member_ids": ids}, &g)
	return g.ID
}

func (u *user) upload(content []byte) string {
	u.e.t.Helper()
	var res struct {
		ID string `json:"id"`
	}
	u.must("POST", "/v1/attachments", content, &res)
	return res.ID
}

func (u *user) post(text string, media ...string) string {
	u.e.t.Helper()
	ms := []map[string]any{}
	for _, id := range media {
		ms = append(ms, map[string]any{"attachment_id": id, "width": 1, "height": 1})
	}
	var m struct {
		ID string `json:"id"`
	}
	u.must("POST", "/v1/moments", map[string]any{"text": text, "media": ms}, &m)
	return m.ID
}

// events 返回用户队列里的全部事件（HTTP 拉取，不 ack）。
func (u *user) events() []map[string]any {
	u.e.t.Helper()
	var res struct {
		Events []map[string]any `json:"events"`
	}
	u.must("GET", "/v1/messages?limit=500", nil, &res)
	return res.Events
}

func (u *user) hasEvent(typ string, match func(map[string]any) bool) bool {
	for _, ev := range u.events() {
		if ev["type"] == typ && (match == nil || match(ev)) {
			return true
		}
	}
	return false
}

// ---------- WebSocket ----------

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func (u *user) dial() *wsClient {
	u.e.t.Helper()
	url := "ws" + strings.TrimPrefix(u.e.apiSrv.URL, "http") + "/v1/ws?token=" + u.Token
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		u.e.t.Fatalf("ws dial: %v", err)
	}
	conn.SetReadLimit(1 << 22)
	u.e.t.Cleanup(func() { conn.CloseNow() })
	w := &wsClient{t: u.e.t, conn: conn}
	w.until("queue_empty", func(t string, _ map[string]any) bool { return t == "queue_empty" })
	return w
}

// next 读取下一帧，返回帧类型与 d；连接关闭时返回关闭码。
func (w *wsClient) next(timeout time.Duration) (typ string, d map[string]any, closeCode int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := w.conn.Read(ctx)
	if err != nil {
		return "", nil, int(websocket.CloseStatus(err)), err
	}
	var f struct {
		T string         `json:"t"`
		D map[string]any `json:"d"`
	}
	json.Unmarshal(data, &f)
	return f.T, f.D, 0, nil
}

func (w *wsClient) until(desc string, pred func(t string, d map[string]any) bool) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		t, d, _, err := w.next(time.Until(deadline))
		if err != nil {
			w.t.Fatalf("等待 %s 失败: %v", desc, err)
		}
		if pred(t, d) {
			return d
		}
	}
	w.t.Fatalf("等待 %s 超时", desc)
	return nil
}

func (w *wsClient) event(typ string) map[string]any {
	w.t.Helper()
	return w.until("事件 "+typ, func(t string, d map[string]any) bool { return t == "event" && d["type"] == typ })
}

// expectKicked 断言收到 logged_out{reason} 后连接以 4001 关闭。
func (w *wsClient) expectKicked(reason string) {
	w.t.Helper()
	d := w.event("logged_out")
	if d["reason"] != reason {
		w.t.Fatalf("logged_out reason 期望 %s，得到 %v", reason, d["reason"])
	}
	if _, has := d["id"]; has {
		w.t.Fatalf("logged_out 不应带 id（不入队列）: %v", d)
	}
	for {
		_, _, code, err := w.next(3 * time.Second)
		if err != nil {
			if code != ws.CloseLoggedOut {
				w.t.Fatalf("关闭码期望 %d，得到 %d (%v)", ws.CloseLoggedOut, code, err)
			}
			return
		}
	}
}
