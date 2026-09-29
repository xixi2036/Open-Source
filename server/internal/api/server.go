// Package api 实现 hush-server 的 HTTP/WebSocket 接口（设计文档第 3 节）。
package api

import (
	"net/http"
	"path/filepath"
	"time"

	"hush/server/internal/mail"
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
	TurnTTL    time.Duration // 默认 24h

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
}

// New 构造完整的 http.Handler（含日志与统一错误体）。
func New(cfg Config) http.Handler {
	if cfg.TurnTTL == 0 {
		cfg.TurnTTL = 24 * time.Hour
	}
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
	mux := http.NewServeMux()
	s.routes(mux)
	return logRequests(jsonFallback(mux))
}

func (s *Server) routes(mux *http.ServeMux) {
	h := func(pattern string, fn authed) { mux.Handle(pattern, s.auth(fn)) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, obj{"ok": true}) })

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
}
