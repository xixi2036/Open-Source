package api

import (
	"context"
	"hash/maphash"
	"log"
	"sync"
	"time"

	"hush/server/internal/store"
)

// 统计增量在内存里累计、定时落库：认证请求、发消息这些热路径上不额外写库。
const (
	StatsFlushInterval = 30 * time.Second
	activityRewrite    = 10 * time.Minute // 同一用户的最近活跃时间最多这么久写一次库
)

type activity struct {
	day       int
	at        int64
	flushedAt int64
	flushDay  int
}

const statShards = 32

type activityShard struct {
	mu    sync.Mutex
	seen  map[string]*activity
	dirty map[string]struct{}
}

// statsRecorder 累计「今日活跃」「每小时入队信封数」「群消息计数」（管理后台 A6 / A8）。只有计数，没有任何消息内容。
// 活跃记录按用户 id 分片：每个认证请求都要记一次，不能让它们争同一把锁。
type statsRecorder struct {
	seed   maphash.Seed
	shards [statShards]activityShard

	mu     sync.Mutex // 保护 hourly / groups（每次发送请求一次，临界区只有两次 map 写）
	hourly map[int64]int64
	groups map[string]store.GroupDelta
}

func newStatsRecorder() *statsRecorder {
	r := &statsRecorder{seed: maphash.MakeSeed(), hourly: map[int64]int64{}, groups: map[string]store.GroupDelta{}}
	for i := range r.shards {
		r.shards[i].seen, r.shards[i].dirty = map[string]*activity{}, map[string]struct{}{}
	}
	return r
}

// touch 记录一次认证请求。
func (s *statsRecorder) touch(uid string, now time.Time) {
	ms, day := now.UnixMilli(), store.DayOf(now)
	sh := &s.shards[maphash.String(s.seed, uid)%statShards]
	sh.mu.Lock()
	a := sh.seen[uid]
	if a == nil {
		a = &activity{}
		sh.seen[uid] = a
	}
	a.day, a.at = day, ms
	if a.flushDay != day || ms-a.flushedAt >= activityRewrite.Milliseconds() {
		sh.dirty[uid] = struct{}{}
	}
	sh.mu.Unlock()
}

// envelopes 记录入队了 n 条信封；gid 非空表示这是一条群消息。
func (s *statsRecorder) envelopes(n int, gid string, now time.Time) {
	if n <= 0 && gid == "" {
		return
	}
	s.mu.Lock()
	if n > 0 {
		s.hourly[now.Unix()/3600] += int64(n)
	}
	if gid != "" {
		d := s.groups[gid]
		d.Messages++
		d.LastAt = now.UnixMilli()
		s.groups[gid] = d
	}
	s.mu.Unlock()
}

// take 取出待落库的增量并清空累计。
func (s *statsRecorder) take(now time.Time) store.StatsBatch {
	s.mu.Lock()
	b := store.StatsBatch{Hourly: s.hourly, Groups: s.groups}
	s.hourly, s.groups = map[int64]int64{}, map[string]store.GroupDelta{}
	s.mu.Unlock()
	// 超过一天没有请求的用户不再占内存
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for uid := range sh.dirty {
			a := sh.seen[uid]
			b.Active = append(b.Active, store.ActivityMark{UserID: uid, Day: a.day, At: a.at})
			a.flushedAt, a.flushDay = a.at, a.day
		}
		sh.dirty = map[string]struct{}{}
		for uid, a := range sh.seen {
			if a.at < cutoff {
				delete(sh.seen, uid)
			}
		}
		sh.mu.Unlock()
	}
	return b
}

// restore 在落库失败时把计数放回去（活跃标记下次请求会重新产生）。
func (s *statsRecorder) restore(b store.StatsBatch) {
	s.mu.Lock()
	for h, n := range b.Hourly {
		s.hourly[h] += n
	}
	for g, d := range b.Groups {
		cur := s.groups[g]
		cur.Messages += d.Messages
		cur.LastAt = max(cur.LastAt, d.LastAt)
		s.groups[g] = cur
	}
	s.mu.Unlock()
	for _, a := range b.Active {
		sh := &s.shards[maphash.String(s.seed, a.UserID)%statShards]
		sh.mu.Lock()
		if e := sh.seen[a.UserID]; e != nil {
			e.flushedAt, e.flushDay = 0, 0
			sh.dirty[a.UserID] = struct{}{}
		}
		sh.mu.Unlock()
	}
}

// FlushStats 把内存里累计的统计增量写入数据库（后台读取仪表盘前也会调用，保证数字是最新的）。
func (s *Server) FlushStats(ctx context.Context) error {
	b := s.stats.take(time.Now())
	if err := s.st.RecordStats(ctx, b); err != nil {
		s.stats.restore(b)
		return err
	}
	return nil
}

// RunStats 定时落库统计增量并清理过旧的明细，直到 ctx 结束；结束前再落库一次。
func (s *Server) RunStats(ctx context.Context) {
	tick := time.NewTicker(StatsFlushInterval)
	defer tick.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.FlushStats(c); err != nil {
				log.Printf("[stats] 退出前落库失败: %v", err)
			}
			cancel()
			return
		case now := <-tick.C:
			if err := s.FlushStats(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[stats] 落库失败: %v", err)
			}
			if now.Sub(lastPrune) >= time.Hour {
				lastPrune = now
				// 活跃明细保留 400 天，小时计数保留 90 天
				if err := s.st.PruneStats(ctx, store.DayOf(now.AddDate(0, 0, -400)), now.Unix()/3600-90*24); err != nil && ctx.Err() == nil {
					log.Printf("[stats] 清理失败: %v", err)
				}
			}
		}
	}
}
