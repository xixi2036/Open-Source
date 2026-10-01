// Package admin 实现 hush-server 的管理后台（docs/superpowers/specs/2026-09-30-admin-and-scale.md 第 1 节）。
//
// 隐私边界：聊天内容端到端加密，后台不提供任何查看聊天内容的入口。这里的接口只读取服务端本来就持有的数据
// （账号资料、好友 / 群成员关系、明文的朋友圈、队列与附件的数量和大小、连接状态）；凡涉及消息的地方只有计数与字节数，
// 没有任何接口返回队列里的信封。
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hush/server/internal/netx"
	"io/fs"
	"log"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"hush/server/internal/api"
	"hush/server/internal/store"
	"hush/server/internal/ws"
)

//go:embed static
var staticFS embed.FS

// 默认的安全参数（A3）。
const (
	DefaultSessionIdle         = 12 * time.Hour
	DefaultLoginPerIPPerMinute = 5
	DefaultMaxLoginFailures    = 10
	DefaultLockDuration        = 15 * time.Minute
	DefaultBcryptCost          = 12

	MinPasswordLen = 12
	maxPasswordLen = 72 // bcrypt 只使用前 72 字节，超出直接拒绝
	cookieName     = "hush_admin"
	csrfHeader     = "X-CSRF-Token"
	maxBody        = 1 << 20
)

// Config 是管理后台的依赖与参数。
type Config struct {
	Store   *store.Store
	Hub     *ws.Hub
	API     *api.Server
	DataDir string
	Version string

	SecureCookie bool   // 强制给会话 Cookie 加 Secure（TLS 由反向代理终结时设置）；直接以 TLS 提供时自动加
	MetricsToken string // 非空时 /metrics 与 /debug/pprof/ 也接受 Authorization: Bearer <token>（供 Prometheus 抓取 / go tool pprof）
	// TrustedProxies：可信反向代理的地址段。配置后，登录限速与审计里的来源 IP 取它们写的 X-Forwarded-For；
	// 未配置时一律用 TCP 对端地址（放在代理后面而不配置的话，所有请求共用代理 IP 的额度）。
	TrustedProxies *netx.Proxies

	// 以下零值取默认值，测试可调整。
	SessionIdle         time.Duration
	LoginPerIPPerMinute int
	MaxLoginFailures    int
	LockDuration        time.Duration
	BcryptCost          int
	Now                 func() time.Time
}

// Server 是管理后台的 HTTP 服务。
type Server struct {
	cfg    Config
	st     *store.Store
	hub    *ws.Hub
	api    *api.Server
	mux    *http.ServeMux
	routes []route

	loginIP   *limiter
	unknown   *failTracker // 不存在的用户名的失败计数（与真实账号表现一致，避免探测用户名）
	dummyHash []byte
	backupMu  sync.Mutex
}

// 角色等级：数值大的包含数值小的权限。
var roleRank = map[string]int{store.AdminViewer: 1, store.AdminModerator: 2, store.AdminOwner: 3}

type ctx struct {
	s     *Server
	w     http.ResponseWriter
	r     *http.Request
	admin store.Admin
	token string // 会话令牌（用于派生 CSRF 令牌）
}

type handler func(c *ctx)

// route 是一条需要登录的管理接口：每条都声明所需的最低角色，由 serve 统一校验会话、角色与 CSRF。
type route struct {
	method  string
	path    string
	minRole string
	fn      handler
}

// New 构造管理后台。
func New(cfg Config) *Server {
	if cfg.SessionIdle <= 0 {
		cfg.SessionIdle = DefaultSessionIdle
	}
	if cfg.LoginPerIPPerMinute <= 0 {
		cfg.LoginPerIPPerMinute = DefaultLoginPerIPPerMinute
	}
	if cfg.MaxLoginFailures <= 0 {
		cfg.MaxLoginFailures = DefaultMaxLoginFailures
	}
	if cfg.LockDuration <= 0 {
		cfg.LockDuration = DefaultLockDuration
	}
	if cfg.BcryptCost <= 0 {
		cfg.BcryptCost = DefaultBcryptCost
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{cfg: cfg, st: cfg.Store, hub: cfg.Hub, api: cfg.API, mux: http.NewServeMux(),
		loginIP: newLimiter(cfg.LoginPerIPPerMinute, time.Minute),
		unknown: &failTracker{m: map[string]*failState{}}}
	s.dummyHash, _ = bcrypt.GenerateFromPassword([]byte("hush-admin-dummy-password"), cfg.BcryptCost)
	s.register()
	return s
}

// Handler 返回带安全响应头的完整 handler。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Printf("[admin] panic %s %s: %v", r.Method, r.URL.Path, v)
				fail(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
		}()
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) handle(method, path, minRole string, fn handler) {
	rt := route{method: method, path: path, minRole: minRole, fn: fn}
	s.routes = append(s.routes, rt)
	s.mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, r *http.Request) { s.serve(rt, w, r) })
}

// RouteInfo 描述一条需要登录的管理接口（测试用它逐条验证鉴权，README 的权限表也与之对应）。
type RouteInfo struct{ Method, Path, MinRole string }

// Routes 返回全部需要登录的管理接口及其最低角色。
func (s *Server) Routes() []RouteInfo {
	out := make([]RouteInfo, len(s.routes))
	for i, r := range s.routes {
		out[i] = RouteInfo{r.method, r.path, r.minRole}
	}
	return out
}

// register 注册全部路由。
func (s *Server) register() {
	const V, M, O = store.AdminViewer, store.AdminModerator, store.AdminOwner

	// 静态页面（go:embed，原生 HTML/CSS/JS）
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, sub, "index.html") })
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", files))

	// 登录与指标：自带鉴权逻辑
	s.mux.HandleFunc("POST /admin/api/login", s.handleLogin)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	// pprof 只挂在管理监听地址上：需要 owner 会话，或 Authorization: Bearer <HUSH_METRICS_TOKEN>
	s.mux.HandleFunc("GET /debug/pprof/", s.pprofAuth(pprof.Index))
	s.mux.HandleFunc("GET /debug/pprof/cmdline", s.pprofAuth(pprof.Cmdline))
	s.mux.HandleFunc("GET /debug/pprof/profile", s.pprofAuth(pprof.Profile))
	s.mux.HandleFunc("GET /debug/pprof/symbol", s.pprofAuth(pprof.Symbol))
	s.mux.HandleFunc("GET /debug/pprof/trace", s.pprofAuth(pprof.Trace))

	// A3 会话
	s.handle("GET", "/admin/api/me", V, (*ctx).me)
	s.handle("POST", "/admin/api/logout", V, (*ctx).logout)
	s.handle("POST", "/admin/api/me/password", V, (*ctx).changeOwnPassword)
	// A6 概览
	s.handle("GET", "/admin/api/dashboard", V, (*ctx).dashboard)
	// A7 用户
	s.handle("GET", "/admin/api/users", V, (*ctx).listUsers)
	s.handle("GET", "/admin/api/users/{id}", V, (*ctx).getUser)
	s.handle("POST", "/admin/api/users/{id}/disable", M, (*ctx).disableUser)
	s.handle("POST", "/admin/api/users/{id}/enable", M, (*ctx).enableUser)
	s.handle("POST", "/admin/api/users/{id}/logout", M, (*ctx).logoutUser)
	s.handle("POST", "/admin/api/users/{id}/reset-profile", M, (*ctx).resetProfile)
	s.handle("DELETE", "/admin/api/users/{id}", M, (*ctx).deleteUser)
	// A8 群组
	s.handle("GET", "/admin/api/groups", V, (*ctx).listGroups)
	s.handle("GET", "/admin/api/groups/{id}", V, (*ctx).getGroup)
	s.handle("DELETE", "/admin/api/groups/{id}", M, (*ctx).dissolveGroup)
	s.handle("DELETE", "/admin/api/groups/{id}/members/{uid}", M, (*ctx).removeMember)
	s.handle("POST", "/admin/api/groups/{id}/owner", M, (*ctx).transferOwner)
	// A9 朋友圈审核
	s.handle("GET", "/admin/api/moments", V, (*ctx).listMoments)
	s.handle("GET", "/admin/api/moments/{id}/comments", V, (*ctx).listComments)
	s.handle("DELETE", "/admin/api/moments/{id}", M, (*ctx).deleteMoment)
	s.handle("DELETE", "/admin/api/comments/{id}", M, (*ctx).deleteComment)
	s.handle("GET", "/admin/api/media/{id}", V, (*ctx).media)
	// A10 举报
	s.handle("GET", "/admin/api/reports", V, (*ctx).listReports)
	s.handle("POST", "/admin/api/reports/{id}/resolve", M, (*ctx).resolveReport)
	// A11 邀请码
	s.handle("GET", "/admin/api/invites", V, (*ctx).listInvites)
	s.handle("POST", "/admin/api/invites", M, (*ctx).createInvites)
	s.handle("POST", "/admin/api/invites/{code}/revoke", M, (*ctx).revokeInvite)
	s.handle("GET", "/admin/api/invites/{code}/uses", V, (*ctx).inviteUses)
	// A12 公告
	s.handle("GET", "/admin/api/announcements", V, (*ctx).listAnnouncements)
	s.handle("POST", "/admin/api/announcements", M, (*ctx).publishAnnouncement)
	s.handle("DELETE", "/admin/api/announcements/{id}", M, (*ctx).deleteAnnouncement)
	// A13 设置
	s.handle("GET", "/admin/api/settings", V, (*ctx).getSettings)
	s.handle("PUT", "/admin/api/settings", O, (*ctx).putSettings)
	// A14 存储与维护
	s.handle("GET", "/admin/api/storage", V, (*ctx).storage)
	s.handle("POST", "/admin/api/maintenance/cleanup", O, (*ctx).cleanup)
	s.handle("POST", "/admin/api/backups", O, (*ctx).createBackup)
	s.handle("GET", "/admin/api/backups/{name}", O, (*ctx).downloadBackup)
	// A15 监控
	s.handle("GET", "/admin/api/monitor", V, (*ctx).monitor)
	// A16 管理员与审计
	s.handle("GET", "/admin/api/admins", O, (*ctx).listAdmins)
	s.handle("POST", "/admin/api/admins", O, (*ctx).createAdmin)
	s.handle("PUT", "/admin/api/admins/{id}", O, (*ctx).updateAdmin)
	s.handle("DELETE", "/admin/api/admins/{id}", O, (*ctx).deleteAdmin)
	s.handle("GET", "/admin/api/audit", O, (*ctx).listAudit)

	// 其余 /admin/api/ 路径：统一 JSON 404
	s.mux.HandleFunc("/admin/api/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "not_found", "接口不存在")
	})
}

// serve 是全部管理接口的统一入口：会话 → 角色 → CSRF（写操作）→ handler。
func (s *Server) serve(rt route, w http.ResponseWriter, r *http.Request) {
	admin, token, ok := s.session(w, r)
	if !ok {
		return
	}
	if roleRank[admin.Role] < roleRank[rt.minRole] {
		fail(w, http.StatusForbidden, "forbidden", "当前角色无权执行此操作")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !sameOrigin(r) {
			fail(w, http.StatusForbidden, "csrf", "请求来源不合法")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(csrfToken(token))) != 1 {
			fail(w, http.StatusForbidden, "csrf", "缺少或无效的 CSRF 令牌，请刷新页面后重试")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	rt.fn(&ctx{s: s, w: w, r: r, admin: admin, token: token})
}

// session 校验会话 Cookie；失败时已写出 401。
func (s *Server) session(w http.ResponseWriter, r *http.Request) (store.Admin, string, bool) {
	ck, err := r.Cookie(cookieName)
	if err != nil || ck.Value == "" {
		fail(w, http.StatusUnauthorized, "unauthorized", "未登录")
		return store.Admin{}, "", false
	}
	a, err := s.st.AdminSession(r.Context(), hashToken(ck.Value), s.cfg.Now().UnixMilli(), s.cfg.SessionIdle)
	if errors.Is(err, store.ErrNotFound) {
		s.clearCookie(w, r)
		fail(w, http.StatusUnauthorized, "unauthorized", "登录已过期，请重新登录")
		return a, "", false
	}
	if err != nil {
		internal(w, r, err)
		return a, "", false
	}
	return a, ck.Value, true
}

// sameOrigin 校验写请求的来源：带了 Origin 头就必须与 Host 一致（同一主机上不同端口的页面是同站但不同源，
// SameSite Cookie 拦不住它们）；没带 Origin 时看 Sec-Fetch-Site。
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	return true // 非浏览器客户端（curl、测试）：仍需通过 CSRF 令牌
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// csrfToken 由会话令牌派生：页面脚本读不到 HttpOnly 的 Cookie，只能从登录响应或 GET /admin/api/me 拿到它。
func csrfToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("hush-admin-csrf:" + sessionToken))
	return hex.EncodeToString(sum[:])
}

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookie || r.TLS != nil})
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookie || r.TLS != nil})
}

// ---------- 通用辅助 ----------

type obj = map[string]any

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, obj{"error": code, "message": msg})
}

func internal(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[admin] %s %s: %v", r.Method, r.URL.Path, err)
	fail(w, http.StatusInternalServerError, "internal", "服务器内部错误")
}

func (c *ctx) ok(v any)                 { writeJSON(c.w, http.StatusOK, v) }
func (c *ctx) bad(msg string)           { fail(c.w, http.StatusBadRequest, "bad_request", msg) }
func (c *ctx) notFound(msg string)      { fail(c.w, http.StatusNotFound, "not_found", msg) }
func (c *ctx) conflict(msg string)      { fail(c.w, http.StatusConflict, "conflict", msg) }
func (c *ctx) internal(err error)       { internal(c.w, c.r, err) }
func (c *ctx) path(name string) string  { return c.r.PathValue(name) }
func (c *ctx) query(name string) string { return strings.TrimSpace(c.r.URL.Query().Get(name)) }
func (c *ctx) now() time.Time           { return c.s.cfg.Now() }
func (c *ctx) storeErr(err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		c.notFound(notFoundMsg)
		return
	}
	c.internal(err)
}

// decode 解析 JSON 请求体；失败时已写出 400。空请求体视为 {}。
func (c *ctx) decode(v any) bool {
	dec := json.NewDecoder(c.r.Body)
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		c.bad("无效的 JSON: " + err.Error())
		return false
	}
	return true
}

// page 解析分页参数：page 从 1 起，size 默认 20、上限 100。
func (c *ctx) page() (page, size int) {
	page, _ = strconv.Atoi(c.query("page"))
	size, _ = strconv.Atoi(c.query("size"))
	if page < 1 {
		page = 1
	}
	if page > 1_000_000 {
		page = 1_000_000
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return
}

func paged(items any, total, page, size int) obj {
	return obj{"items": items, "total": total, "page": page, "size": size}
}

// audit 生成一条审计记录（写操作都要落一条：时间、管理员、动作、对象、参数摘要、来源 IP）。
func (c *ctx) audit(action, target, detail string) store.AuditEntry {
	if len(detail) > 1000 {
		detail = detail[:1000] + "…"
	}
	return store.AuditEntry{TS: c.now().UnixMilli(), AdminID: c.admin.ID, AdminName: c.admin.Username,
		Action: action, Target: target, Detail: detail, IP: c.s.clientIP(c.r)}
}

// writeAudit 在业务写入已经完成之后单独落审计（业务不在事务里的场合，如清理、备份）。
func (c *ctx) writeAudit(action, target, detail string) {
	if err := c.s.st.AppendAudit(c.r.Context(), c.audit(action, target, detail)); err != nil {
		log.Printf("[admin] 审计日志写入失败 action=%s target=%s: %v", action, target, err)
	}
}

// clientIP 返回客户端 IP：默认是 TCP 对端地址；配置了可信代理且请求确实来自它们时取 X-Forwarded-For。
func (s *Server) clientIP(r *http.Request) string { return s.cfg.TrustedProxies.ClientIP(r) }

// jsonDetail 把参数摘要编码成紧凑 JSON（审计 detail 用；调用方不得放入密码等敏感值）。
func jsonDetail(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---------- 限速 ----------

// limiter 是固定窗口计数限速器。
type limiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	m      map[string]*failState
}

type failState struct {
	start time.Time
	n     int
	until time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, m: map[string]*failState{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil || now.Sub(b.start) >= l.window {
		if len(l.m) > 10000 {
			for k, v := range l.m {
				if now.Sub(v.start) >= l.window {
					delete(l.m, k)
				}
			}
		}
		b = &failState{start: now}
		l.m[key] = b
	}
	if b.n >= l.limit {
		return false
	}
	b.n++
	return true
}

// failTracker 在内存里给「不存在的用户名」记连续失败与锁定，使其对外表现与真实账号一致。
type failTracker struct {
	mu sync.Mutex
	m  map[string]*failState
}

func (f *failTracker) locked(name string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.m[name]
	return st != nil && now.Before(st.until)
}

func (f *failTracker) fail(name string, max int, lock time.Duration, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) > 10000 {
		for k, v := range f.m {
			if now.After(v.until) && now.Sub(v.start) > lock {
				delete(f.m, k)
			}
		}
		if len(f.m) > 10000 {
			return
		}
	}
	st := f.m[name]
	if st == nil {
		st = &failState{}
		f.m[name] = st
	}
	st.start = now
	st.n++
	if st.n >= max {
		st.n, st.until = 0, now.Add(lock)
	}
}

// tokenAuthed 判断请求是否带了正确的 HUSH_METRICS_TOKEN。
func (s *Server) tokenAuthed(r *http.Request) bool {
	tok := s.cfg.MetricsToken
	if tok == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	return len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[7:])), []byte(tok)) == 1
}

// pprofAuth 保护 pprof：owner 会话或指标令牌。profile / trace 可能运行几十秒，清除本请求的写超时。
func (s *Server) pprofAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenAuthed(r) {
			a, _, ok := s.session(w, r)
			if !ok {
				return
			}
			if roleRank[a.Role] < roleRank[store.AdminOwner] {
				fail(w, http.StatusForbidden, "forbidden", "需要 owner 权限")
				return
			}
		}
		// pprof 的索引页带内联样式；其余安全头不变
		w.Header().Del("Content-Security-Policy")
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		fn(w, r)
	}
}
