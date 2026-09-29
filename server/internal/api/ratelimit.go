package api

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// windowLimiter 是简单的固定窗口计数限速器（进程内，单实例部署足够）。
type windowLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, buckets: map[string]*bucket{}}
}

// allow 记一次调用并返回是否允许。
func (l *windowLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		if len(l.buckets) > 10000 {
			l.gc(now)
		}
		b = &bucket{start: now}
		l.buckets[key] = b
	}
	if b.n >= l.limit {
		return false
	}
	b.n++
	return true
}

// gc 清理已过期的窗口（调用方持锁）。
func (l *windowLimiter) gc(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.start) >= l.window {
			delete(l.buckets, k)
		}
	}
}

// allowWrite 对会向他人队列写事件的写操作按 (用户, 操作类别) 限速；超出时已写出 429 并返回 false。
// 事件合并（见 store.QueueInsert.CoalesceKey）限制了队列堆积，这里再限制写库与扇出的频率。
func (s *Server) allowWrite(w http.ResponseWriter, uid, action string) bool {
	if s.writeLimiter.allow(uid+"|"+action, time.Now()) {
		return true
	}
	log.Printf("[ratelimit] user=%s %s 过于频繁", uid, action)
	fail(w, codeRateLimited, "操作过于频繁，请稍后再试")
	return false
}
