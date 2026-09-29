package api

import (
	"context"
	"sync"
	"time"

	"hush/server/internal/store"
)

// queueLimits 是接收方队列配额。策略：队列满（条数或字节超限）时**拒收新信封**，不丢弃已有数据；
// 服务端事件不受此限制。检查在入队事务内完成（见 store.Enqueue）。
func (s *Server) queueLimits() store.QueueLimits {
	return store.QueueLimits{MaxItems: s.cfg.QueueMaxItems, MaxBytes: s.cfg.QueueMaxBytes}
}

// uploadReservations 记录各用户正在进行中的上传所预占的字节数。
// 上传内容要先完整读入（可能持续数分钟），元数据写库在最后；只看数据库里的已用量，
// N 个并发上传会看到同一个 used 而一起突破配额。预占让并发上传彼此可见。进程内状态，单实例部署足够。
type uploadReservations struct {
	mu       sync.Mutex
	reserved map[string]int64
}

type uploadReservation struct {
	u    *uploadReservations
	s    *Server
	uid  string
	size int64
}

// reserve 在配额允许时为 uid 预占 n 字节；超出配额返回 nil。
func (u *uploadReservations) reserve(ctx context.Context, s *Server, uid string, n int64) (*uploadReservation, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	used, err := s.uploadedToday(ctx, uid)
	if err != nil {
		return nil, err
	}
	if used+u.reserved[uid]+n > s.cfg.DailyUploadBytes {
		return nil, nil
	}
	u.reserved[uid] += n
	return &uploadReservation{u: u, s: s, uid: uid, size: n}, nil
}

// resize 把预占改为实际大小；改后超出配额则保持原预占并返回 false。
func (r *uploadReservation) resize(ctx context.Context, n int64) (bool, error) {
	u := r.u
	u.mu.Lock()
	defer u.mu.Unlock()
	used, err := r.s.uploadedToday(ctx, r.uid)
	if err != nil {
		return false, err
	}
	if used+u.reserved[r.uid]-r.size+n > r.s.cfg.DailyUploadBytes {
		return false, nil
	}
	u.reserved[r.uid] += n - r.size
	r.size = n
	return true, nil
}

// release 释放预占（元数据已写库或上传失败之后）。
func (r *uploadReservation) release() {
	u := r.u
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reserved[r.uid] -= r.size; u.reserved[r.uid] <= 0 {
		delete(u.reserved, r.uid)
	}
}

func (s *Server) uploadedToday(ctx context.Context, uid string) (int64, error) {
	return s.st.UploadedBytesSince(ctx, uid, time.Now().Add(-24*time.Hour).UnixMilli())
}
