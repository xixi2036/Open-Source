// Package api 实现 hush-server 的 HTTP/WebSocket 接口（设计文档第 3 节）。
package api

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"hush/server/internal/mail"
	"hush/server/internal/metrics"
	"hush/server/internal/netx"
	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// Config 是 API 服务依赖与配置。
type Config struct {
	Store   *store.Store
	Hub     *ws.Hub
	Mailer  mail.Sender // nil 表示未配置邮件服务
	DataDir string
	DevCode string // 非空时对任意邮箱永远有效

	TurnSecret string
	TurnPort   int           // 实际在运行的 TURN 端口；0 表示 TURN 不可用（/v1/calls/turn 返回空 urls）
	TurnTTL    time.Duration // TURN 凭证有效期，默认 6h

	MaxAttachmentBytes int64 // 默认 100MB（测试可调小）

	// TokenTTL 是令牌滑动过期时长：超过这么久未使用的令牌失效（401）。0 表示永不过期（api 包缺省值；
	// hush-server 的 --token-ttl 默认 90 天）。
	TokenTTL time.Duration

	ClaimPerMinute int // 每对 (调用者, 目标) 每分钟可 claim 的次数，默认 10

	// 会向他人队列写事件的写接口（好友申请、改资料、建群/改群/加人/踢人/设管理员、点赞、评论），
	// 每个用户每类操作每分钟的次数上限，默认 30。
	WritesPerMinute int
	// 未认证的登录接口按对端 IP 每分钟的次数上限：/v1/auth/code 默认 10，/v1/auth/verify 默认 30。
	AuthCodePerIPPerMinute   int
	AuthVerifyPerIPPerMinute int

	QueueMaxItems    int64         // 每个接收方队列最多条数，默认 10000
	QueueMaxBytes    int64         // 每个接收方队列最多负载字节，默认 200MB
	DailyUploadBytes int64         // 每用户 24 小时内上传附件总量，默认 2GB
	BlobTimeout      time.Duration // 附件上传/下载路由的读写超时，默认 10 分钟

	// 以下为管理后台相关（2026-09-30），零值即现有行为：开放注册、不限邮箱域名。
	RegistrationMode string   // RegOpen（默认）| RegInvite | RegClosed
	EmailDomains     []string // 新用户邮箱域名白名单；空表示不限
	ReportsPerHour   int      // POST /v1/reports 每用户每小时次数，默认 20
	BackupKeep       int      // 在线备份保留份数，默认 7

	// 发送侧限速与子配额（2026-09-30 缺陷修复 A1 / A2 / A5），零值取默认。
	SendOlmPerMinute    int   // 每用户每分钟单聊信封数（按接收方条目计），默认 3000
	SendGroupPerMinute  int   // 每用户每分钟群消息数，默认 300
	SendBytesPerMinute  int64 // 每用户每分钟消息正文字节，默认 64MB
	SenderQueueMaxItems int64 // 单个发送方在某接收方队列里的上限，默认 1000 条
	SenderQueueMaxBytes int64 // 默认 20MB
	SearchPerMinute     int   // 每用户每分钟按邮箱搜索次数，默认 20
	MomentsPerDay       int   // 每用户 24 小时发布朋友圈条数，默认 200

	// 资源上限（P5）。
	MaxInflight int // 同时处理的普通请求数上限（不含 WebSocket 与附件上传下载），超出返回 503；默认 1024
	MaxUploads  int // 同时进行的附件上传数上限，超出返回 503；默认 32

	// TrustedProxies 是可信反向代理的地址段；配置后才采信它们写的 X-Forwarded-For（B1）。nil 表示直接对外。
	TrustedProxies *netx.Proxies

	Metrics *metrics.Registry // nil 时内部新建
}

// 默认配额。
const (
	DefaultQueueMaxItems    = 10000
	DefaultQueueMaxBytes    = 200 << 20
	DefaultDailyUploadBytes = 2 << 30
	DefaultBlobTimeout      = 10 * time.Minute

	DefaultTokenTTL = 90 * 24 * time.Hour // hush-server --token-ttl 的默认值

	DefaultClaimPerMinute           = 10
	DefaultWritesPerMinute          = 30
	DefaultAuthCodePerIPPerMinute   = 10
	DefaultAuthVerifyPerIPPerMinute = 30
	DefaultReportsPerHour           = 20
	DefaultBackupKeep               = 7

	DefaultSendOlmPerMinute    = 3000
	DefaultSendGroupPerMinute  = 300
	DefaultSendBytesPerMinute  = 64 << 20
	DefaultSenderQueueMaxItems = 1000
	DefaultSenderQueueMaxBytes = 20 << 20
	DefaultSearchPerMinute     = 20
	DefaultMomentsPerDay       = 200
	DefaultMaxInflight         = 1024
	DefaultMaxUploads          = 32
	DefaultTurnTTL             = 6 * time.Hour
)

// Server 持有全部 handler 依赖。
type Server struct {
	cfg           Config
	st            *store.Store
	hub           *ws.Hub
	attachmentDir string
	claimLimiter  *windowLimiter
	writeLimiter  *windowLimiter
	uploads       *uploadReservations

	authCodeLimiter, authVerifyLimiter *windowLimiter
	reportLimiter                      *windowLimiter
	sendOlmLimiter, sendGroupLimiter   *windowLimiter
	sendBytesLimiter                   *windowLimiter
	searchLimiter                      *windowLimiter

	inflight chan struct{} // 普通请求的并发名额
	uploads2 chan struct{} // 附件上传的并发名额

	defaults Settings                 // 启动参数决定的默认值
	live     atomic.Pointer[Settings] // 当前生效的设置（后台可在线修改）
	draining atomic.Bool
	stats    *statsRecorder
	metrics  *metrics.Registry
	handler  http.Handler
}

// New 构造完整的 http.Handler（含日志与统一错误体）。
func New(cfg Config) http.Handler { return NewServer(cfg).Handler() }

// Handler 返回完整的 http.Handler。
func (s *Server) Handler() http.Handler { return s.handler }

// Metrics 返回请求指标表。
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// NewServer 构造 Server（管理后台需要它来读取 / 修改在线设置、读取指标）。
func NewServer(cfg Config) *Server {
	if cfg.TurnTTL == 0 {
		cfg.TurnTTL = DefaultTurnTTL
	}
	defInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def64 := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	defInt(&cfg.SendOlmPerMinute, DefaultSendOlmPerMinute)
	defInt(&cfg.SendGroupPerMinute, DefaultSendGroupPerMinute)
	def64(&cfg.SendBytesPerMinute, DefaultSendBytesPerMinute)
	def64(&cfg.SenderQueueMaxItems, DefaultSenderQueueMaxItems)
	def64(&cfg.SenderQueueMaxBytes, DefaultSenderQueueMaxBytes)
	defInt(&cfg.SearchPerMinute, DefaultSearchPerMinute)
	defInt(&cfg.MomentsPerDay, DefaultMomentsPerDay)
	defInt(&cfg.MaxInflight, DefaultMaxInflight)
	defInt(&cfg.MaxUploads, DefaultMaxUploads)
	if cfg.MaxAttachmentBytes <= 0 {
		cfg.MaxAttachmentBytes = MaxAttachmentSize
	}
	if cfg.ClaimPerMinute <= 0 {
		cfg.ClaimPerMinute = DefaultClaimPerMinute
	}
	if cfg.QueueMaxItems <= 0 {
		cfg.QueueMaxItems = DefaultQueueMaxItems
	}
	if cfg.QueueMaxBytes <= 0 {
		cfg.QueueMaxBytes = DefaultQueueMaxBytes
	}
	if cfg.DailyUploadBytes <= 0 {
		cfg.DailyUploadBytes = DefaultDailyUploadBytes
	}
	if cfg.BlobTimeout <= 0 {
		cfg.BlobTimeout = DefaultBlobTimeout
	}
	if cfg.WritesPerMinute <= 0 {
		cfg.WritesPerMinute = DefaultWritesPerMinute
	}
	if cfg.AuthCodePerIPPerMinute <= 0 {
		cfg.AuthCodePerIPPerMinute = DefaultAuthCodePerIPPerMinute
	}
	if cfg.AuthVerifyPerIPPerMinute <= 0 {
		cfg.AuthVerifyPerIPPerMinute = DefaultAuthVerifyPerIPPerMinute
	}
	s := &Server{cfg: cfg, st: cfg.Store, hub: cfg.Hub, attachmentDir: filepath.Join(cfg.DataDir, "attachments"),
		claimLimiter:      newWindowLimiter(cfg.ClaimPerMinute, time.Minute),
		writeLimiter:      newWindowLimiter(cfg.WritesPerMinute, time.Minute),
		authCodeLimiter:   newWindowLimiter(cfg.AuthCodePerIPPerMinute, time.Minute),
		authVerifyLimiter: newWindowLimiter(cfg.AuthVerifyPerIPPerMinute, time.Minute),
		uploads:           &uploadReservations{reserved: map[string]int64{}}}
	if cfg.RegistrationMode == "" {
		cfg.RegistrationMode = RegOpen
	}
	if cfg.ReportsPerHour <= 0 {
		cfg.ReportsPerHour = DefaultReportsPerHour
	}
	if cfg.BackupKeep <= 0 {
		cfg.BackupKeep = DefaultBackupKeep
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	s.cfg = cfg
	s.reportLimiter = newWindowLimiter(cfg.ReportsPerHour, time.Hour)
	s.sendOlmLimiter = newWindowLimiter(cfg.SendOlmPerMinute, time.Minute)
	s.sendGroupLimiter = newWindowLimiter(cfg.SendGroupPerMinute, time.Minute)
	s.sendBytesLimiter = newWindowLimiter(0, time.Minute)
	s.searchLimiter = newWindowLimiter(cfg.SearchPerMinute, time.Minute)
	s.inflight = make(chan struct{}, cfg.MaxInflight)
	s.uploads2 = make(chan struct{}, cfg.MaxUploads)
	s.stats = newStatsRecorder()
	s.metrics = cfg.Metrics
	s.defaults = Settings{
		RegistrationMode: cfg.RegistrationMode, EmailDomains: cfg.EmailDomains,
		ClaimPerMinute: cfg.ClaimPerMinute, WritesPerMinute: cfg.WritesPerMinute,
		AuthCodePerIPPerMinute: cfg.AuthCodePerIPPerMinute, AuthVerifyPerIPPerMinute: cfg.AuthVerifyPerIPPerMinute,
		ReportsPerHour:   cfg.ReportsPerHour,
		SendOlmPerMinute: cfg.SendOlmPerMinute, SendGroupPerMinute: cfg.SendGroupPerMinute, SendBytesPerMinute: cfg.SendBytesPerMinute,
		SearchPerMinute: cfg.SearchPerMinute, MomentsPerDay: cfg.MomentsPerDay,
		SenderQueueMaxItems: cfg.SenderQueueMaxItems, SenderQueueMaxBytes: cfg.SenderQueueMaxBytes,
		QueueMaxItems: cfg.QueueMaxItems, QueueMaxBytes: cfg.QueueMaxBytes, DailyUploadBytes: cfg.DailyUploadBytes,
		TokenTTL: cfg.TokenTTL, BackupKeep: cfg.BackupKeep,
	}
	s.ApplySettings(s.defaults)
	mux := http.NewServeMux()
	s.routes(mux)
	s.handler = logRequests(s.limitInflight(jsonFallback(mux)), s.metrics)
	return s
}

// limitInflight 限制同时处理的请求数：名额用完时立刻返回 503（带 Retry-After），而不是让请求无限堆积、把内存和数据库拖垮。
// WebSocket（长连接，另有连接数上限）、附件上传下载（耗时长，上传另有并发上限）、健康检查不占名额。
func (s *Server) limitInflight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v1/ws" || p == "/healthz" || p == "/readyz" || len(p) >= 15 && p[:15] == "/v1/attachments" {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.inflight <- struct{}{}:
			defer func() { <-s.inflight }()
			next.ServeHTTP(w, r)
		default:
			s.metrics.Add("overload_rejected", 1)
			overloaded(w)
		}
	})
}

func overloaded(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	fail(w, codeUnavailable, "服务器繁忙，请稍后重试")
}

// handleReady 是就绪探针：查一次数据库。/healthz 只表示进程活着，/readyz 表示能正常服务（停机过程中返回 503）。
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.draining.Load() {
		fail(w, codeUnavailable, "正在停机")
		return
	}
	if err := s.st.Ping(ctx); err != nil {
		fail(w, codeUnavailable, "数据库不可用")
		return
	}
	writeJSON(w, http.StatusOK, obj{"ok": true})
}

// SetDraining 标记服务端正在停机：/readyz 开始返回 503，负载均衡器据此摘流。
func (s *Server) SetDraining() { s.draining.Store(true) }

func (s *Server) routes(mux *http.ServeMux) {
	h := func(pattern string, fn authed) { mux.Handle(pattern, s.auth(fn)) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, obj{"ok": true}) })
	mux.HandleFunc("GET /readyz", s.handleReady)

	// 3.1 账号
	mux.HandleFunc("POST /v1/auth/code", s.handleRequestCode)
	mux.HandleFunc("POST /v1/auth/verify", s.handleVerify)
	h("POST /v1/auth/logout", s.handleLogout)
	h("GET /v1/me", s.handleMe)
	h("PUT /v1/me/profile", s.handleUpdateProfile)
	h("GET /v1/users/search", s.handleSearchUser)
	h("GET /v1/users/{id}", s.handleGetUser)

	// 3.2 预密钥
	h("PUT /v1/keys", s.handleUploadKeys)
	h("GET /v1/keys/count", s.handleKeyCount)
	h("POST /v1/keys/claim/{user_id}", s.handleClaimKey)
	h("GET /v1/keys/identity/{user_id}", s.handleGetIdentity)

	// 3.3 消息
	h("POST /v1/messages", s.handleSendOlm)
	h("GET /v1/messages", s.handleFetchMessages)
	h("POST /v1/messages/ack", s.handleAck)
	h("POST /v1/groups/{id}/messages", s.handleSendMegolm)

	// 3.4 WebSocket
	h("GET /v1/ws", s.handleWS)

	// 3.5 好友与拉黑
	h("POST /v1/friends/requests", s.handleSendFriendRequest)
	h("GET /v1/friends/requests", s.handleListFriendRequests)
	h("POST /v1/friends/requests/{id}/accept", s.handleAcceptFriendRequest)
	h("POST /v1/friends/requests/{id}/reject", s.handleRejectFriendRequest)
	h("GET /v1/friends", s.handleListFriends)
	h("DELETE /v1/friends/{user_id}", s.handleRemoveFriend)
	h("GET /v1/blocks", s.handleListBlocks)
	h("POST /v1/blocks/{user_id}", s.handleBlock)
	h("DELETE /v1/blocks/{user_id}", s.handleUnblock)

	// 3.6 群组
	h("POST /v1/groups", s.handleCreateGroup)
	h("GET /v1/groups", s.handleListGroups)
	h("GET /v1/groups/{id}", s.handleGetGroup)
	h("PUT /v1/groups/{id}", s.handleUpdateGroup)
	h("POST /v1/groups/{id}/members", s.handleAddMembers)
	h("DELETE /v1/groups/{id}/members/{uid}", s.handleRemoveMember)
	h("PUT /v1/groups/{id}/admins/{uid}", s.handleSetAdmin)

	// 3.7 附件
	h("POST /v1/attachments", s.handleUploadAttachment)
	h("GET /v1/attachments/{id}", s.handleDownloadAttachment)

	// 3.8 朋友圈
	h("POST /v1/moments", s.handleCreateMoment)
	h("GET /v1/moments/feed", s.handleFeed)
	h("GET /v1/moments/user/{uid}", s.handleUserMoments)
	h("GET /v1/moments/notifications", s.handleNotifications)
	h("GET /v1/moments/{id}", s.handleGetMoment)
	h("DELETE /v1/moments/{id}", s.handleDeleteMoment)
	h("POST /v1/moments/{id}/like", s.handleLike)
	h("POST /v1/moments/{id}/comments", s.handleComment)
	// "DELETE /v1/moments/{id}/like" 与 "DELETE /v1/moments/comments/{cid}" 在 ServeMux 中互相冲突，
	// 统一注册为两段通配后手动分派。
	h("DELETE /v1/moments/{a}/{b}", s.handleMomentDelete2)

	// 3.9 通话
	h("GET /v1/calls/turn", s.handleTurn)

	// 举报与系统公告（管理后台 A10 / A12）
	h("POST /v1/reports", s.handleReport)
	h("GET /v1/announcements", s.handleAnnouncements)
}
