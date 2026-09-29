package api

import (
	"log"
	"net/http"
	"time"
)

// Timeouts 是 http.Server 的全局超时。附件上传/下载与 WebSocket 路由在 handler 内单独放宽。
type Timeouts struct {
	ReadHeader time.Duration
	Read       time.Duration
	Write      time.Duration
	Idle       time.Duration
}

// DefaultTimeouts 是生产默认值。
var DefaultTimeouts = Timeouts{ReadHeader: 15 * time.Second, Read: 60 * time.Second, Write: 60 * time.Second, Idle: 120 * time.Second}

// NewHTTPServer 构造带超时的 http.Server。
func NewHTTPServer(addr string, h http.Handler, t Timeouts) *http.Server {
	srv := &http.Server{Addr: addr, Handler: h}
	ApplyTimeouts(srv, t)
	return srv
}

// ApplyTimeouts 把超时写入 srv（测试用 httptest 时也可调用）。
func ApplyTimeouts(srv *http.Server, t Timeouts) {
	srv.ReadHeaderTimeout = t.ReadHeader
	srv.ReadTimeout = t.Read
	srv.WriteTimeout = t.Write
	srv.IdleTimeout = t.Idle
}

// extendDeadlines 放宽本请求的读写截止时间（d=0 表示清除，用于 WebSocket 长连接）。
func extendDeadlines(w http.ResponseWriter, read, write bool, d time.Duration) {
	rc := http.NewResponseController(w)
	var at time.Time
	if d > 0 {
		at = time.Now().Add(d)
	}
	if read {
		if err := rc.SetReadDeadline(at); err != nil {
			log.Printf("[http] 设置读超时失败: %v", err)
		}
	}
	if write {
		if err := rc.SetWriteDeadline(at); err != nil {
			log.Printf("[http] 设置写超时失败: %v", err)
		}
	}
}
