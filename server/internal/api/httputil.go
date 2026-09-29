package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hush/server/internal/store"
)

type obj = map[string]any

const maxJSONBody = 1 << 20 // 1MB，批量消息另行放宽

// 错误码（契约 3 通用约定）。
const (
	codeBadRequest   = "bad_request"
	codeUnauthorized = "unauthorized"
	codeForbidden    = "forbidden"
	codeNotFound     = "not_found"
	codeNotFriends   = "not_friends"
	codeBlocked      = "blocked"
	codeRateLimited  = "rate_limited"
	codeConflict     = "conflict"
	codeTooLarge     = "too_large"
	codeInternal     = "internal"
	codeMethod       = "method_not_allowed"
)

var statusByCode = map[string]int{
	codeBadRequest:   http.StatusBadRequest,
	codeUnauthorized: http.StatusUnauthorized,
	codeForbidden:    http.StatusForbidden,
	codeNotFound:     http.StatusNotFound,
	codeNotFriends:   http.StatusForbidden,
	codeBlocked:      http.StatusForbidden,
	codeRateLimited:  http.StatusTooManyRequests,
	codeConflict:     http.StatusConflict,
	codeTooLarge:     http.StatusRequestEntityTooLarge,
	codeInternal:     http.StatusInternalServerError,
	codeMethod:       http.StatusMethodNotAllowed,
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // 密文/信封原样输出，不把 <>& 转义成 \u003c 等
	enc.Encode(v)
}

func ok(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

// fail 以统一错误体返回：{"error":"<code>","message":"..."}。
func fail(w http.ResponseWriter, code, msg string) {
	status, found := statusByCode[code]
	if !found {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, obj{"error": code, "message": msg})
}

// internalErr 记录内部错误并返回 500。
func internalErr(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[error] %s %s: %v", r.Method, r.URL.Path, err)
	fail(w, codeInternal, "服务器内部错误")
}

// storeErr 把常见存储错误映射为错误码。
func storeErr(w http.ResponseWriter, r *http.Request, err error, notFoundMsg string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, codeNotFound, notFoundMsg)
	case errors.Is(err, store.ErrConflict):
		fail(w, codeConflict, "与现有数据冲突")
	default:
		internalErr(w, r, err)
	}
}

// decode 解析 JSON 请求体；失败时已写出错误并返回 false。
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if limit <= 0 {
		limit = maxJSONBody
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, codeTooLarge, "请求体过大")
			return false
		}
		fail(w, codeBadRequest, "无效的 JSON: "+err.Error())
		return false
	}
	return true
}

// queryInt 读取整数查询参数，缺省返回 def。
func queryInt(r *http.Request, name string, def int64) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// pageParams 解析 before（毫秒，默认无穷大）、可选的 before_id（同一毫秒内的复合游标）与 limit（默认 def，上限 max）。
func pageParams(w http.ResponseWriter, r *http.Request, def, max int64) (cur store.Cursor, limit int, okk bool) {
	before, limit, okk := pageBefore(w, r, def, max)
	if !okk {
		return store.Cursor{}, 0, false
	}
	beforeID := r.URL.Query().Get("before_id")
	if len(beforeID) > 64 {
		fail(w, codeBadRequest, "before_id 无效")
		return store.Cursor{}, 0, false
	}
	return store.Cursor{Before: before, BeforeID: beforeID}, limit, true
}

// noCursor 是缺省的 before（比任何毫秒时间戳都大，表示从最新开始）。
const noCursor = 1 << 62

func pageBefore(w http.ResponseWriter, r *http.Request, def, max int64) (before int64, limit int, okk bool) {
	before, ok1 := queryInt(r, "before", noCursor)
	lim, ok2 := queryInt(r, "limit", def)
	if !ok1 || !ok2 || lim <= 0 {
		fail(w, codeBadRequest, "before/limit 参数无效")
		return 0, 0, false
	}
	if before <= 0 {
		before = noCursor
	}
	if lim > max {
		lim = max
	}
	return before, int(lim), true
}

// ---------- 鉴权 ----------

type ctxKey int

const tokenHashKey ctxKey = 1

type authed func(w http.ResponseWriter, r *http.Request, uid string)

// tokenHash 返回本请求所用 token 的哈希（由 auth 中间件放入 ctx）。
func tokenHash(r *http.Request) string {
	th, _ := r.Context().Value(tokenHashKey).(string)
	return th
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// bearerToken 取 Authorization 头里的 token。只有 WebSocket（GET /v1/ws）也接受 ?token=（契约只为 WS 放开）：
// 其他 URL 一旦带上 token，就可能被代理、系统日志或分享链接记录下来。
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/ws" {
		return r.URL.Query().Get("token")
	}
	return ""
}

func (s *Server) auth(fn authed) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			fail(w, codeUnauthorized, "缺少 token")
			return
		}
		th := hashToken(tok)
		uid, err := s.st.Authenticate(r.Context(), th, store.NowMillis(), s.cfg.TokenTTL)
		if errors.Is(err, store.ErrNotFound) {
			fail(w, codeUnauthorized, "token 无效或已失效")
			return
		}
		if err != nil {
			internalErr(w, r, err)
			return
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), tokenHashKey, th)), uid)
	})
}

// ---------- 中间件 ----------

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	if s.status == 0 {
		s.status = http.StatusSwitchingProtocols
	}
	return hj.Hijack()
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// logRequests 记录「方法 路径 状态 响应字节 耗时」。查询串不记录（可能含 token），附件 id 截断（见 maskID）。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		log.Printf("[http] %s %s %d %dB %s", r.Method, logPath(r.URL.Path), rec.status, rec.bytes, time.Since(start).Round(time.Microsecond))
	})
}

// maskID 截断日志里的附件 id：凭 id 即可下载附件（API_NOTES 3.7），完整 id 不能出现在日志里。
func maskID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// logPath 返回用于日志的路径：附件路径中的 id 截断。
func logPath(p string) string {
	const prefix = "/v1/attachments/"
	if strings.HasPrefix(p, prefix) {
		return prefix + maskID(p[len(prefix):])
	}
	return p
}

// jsonFallback 让未匹配路由（404/405）也返回统一 JSON 错误体。
func jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &probeWriter{header: http.Header{}}
		mux.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			if allow := probe.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			fail(w, codeMethod, "不支持的请求方法")
			return
		}
		fail(w, codeNotFound, "接口不存在")
	})
}

type probeWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (p *probeWriter) Header() http.Header { return p.header }
func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return p.buf.Write(b)
}
func (p *probeWriter) WriteHeader(code int) { p.status = code }
