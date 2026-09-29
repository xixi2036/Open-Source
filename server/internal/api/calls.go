package api

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"hush/server/internal/turn"
)

// GET /v1/calls/turn → {urls, username, credential, ttl}
// URL 中的主机取客户端访问本服务所用的主机名（保证客户端可达），端口为 --turn-port。
// TurnPort 必须是实际启动成功的端口（main 在 TURN 启动失败时传 0）。
func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request, uid string) {
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}
	username, credential := turn.Credentials(s.cfg.TurnSecret, uid, s.cfg.TurnTTL, time.Now())
	// TURN 未启用（--turn-port 0）或启动失败时 TurnPort=0：返回空的 urls，客户端只用直连候选，
	// 而不是先去连一个不存在的中继、等到 ICE 超时。
	urls := []string{}
	if s.cfg.TurnPort > 0 {
		urls = []string{
			fmt.Sprintf("stun:%s:%d", host, s.cfg.TurnPort),
			fmt.Sprintf("turn:%s:%d?transport=udp", host, s.cfg.TurnPort),
		}
	}
	ok(w, obj{
		"urls":       urls,
		"username":   username,
		"credential": credential,
		"ttl":        int64(s.cfg.TurnTTL / time.Second),
	})
}
