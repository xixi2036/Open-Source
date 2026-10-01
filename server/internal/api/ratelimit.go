package api

import (
	"hash/maphash"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"hush/server/internal/logx"
)

const limiterShards = 32

// windowLimiter 是简单的固定窗口计数限速器（进程内，单实例部署足够）。按 key 分片加锁，热路径上不争用一把全局锁。
type windowLimiter struct {
	limit  atomic.Int64
	window time.Duration
	seed   maphash.Seed
	shards [limiterShards]limiterShard
}

type limiterShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int64
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	l := &windowLimiter{window: window, seed: maphash.MakeSeed()}
	l.limit.Store(int64(limit))
	for i := range l.shards {
		l.shards[i].buckets = map[string]*bucket{}
	}
	return l
}

// setLimit 在线调整上限（管理后台修改限速参数）；已有窗口里的计数保留。
func (l *windowLimiter) setLimit(n int64) { l.limit.Store(n) }

// allow 记一次调用并返回是否允许。
func (l *windowLimiter) allow(key string, now time.Time) bool { return l.allowN(key, 1, now) }

// allowN 记 n 个单位并返回是否允许（不允许时不计数）。窗口内的第一次调用总是允许——
// 单次请求本身就超过上限时（上限被调得很小）不至于永远发不出去。
func (l *windowLimiter) allowN(key string, n int64, now time.Time) bool {
	s := &l.shards[maphash.String(l.seed, key)%limiterShards]
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		if len(s.buckets) > 4096 {
			for k, old := range s.buckets { // 清理已过期的窗口
				if now.Sub(old.start) >= l.window {
					delete(s.buckets, k)
				}
			}
		}
		b = &bucket{start: now}
		s.buckets[key] = b
	}
	if b.n > 0 && b.n+n > l.limit.Load() {
		return false
	}
	b.n += n
	return true
}

// allowWrite 对会向他人队列写事件的写操作按 (用户, 操作类别) 限速；超出时已写出 429 并返回 false。
// 事件合并（见 store.QueueInsert.CoalesceKey）限制了队列堆积，这里再限制写库与扇出的频率。
func (s *Server) allowWrite(w http.ResponseWriter, uid, action string) bool {
	if s.writeLimiter.allow(uid+"|"+action, time.Now()) {
		return true
	}
	logx.Debugf("[ratelimit] user=%s %s 过于频繁", uid, action)
	fail(w, codeRateLimited, "操作过于频繁，请稍后再试")
	return false
}
