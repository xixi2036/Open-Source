// Package ws 负责在线连接管理与投递：信封/事件按接收方持久化进队列，在线则经 WebSocket 推送。
package ws

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	"hush/server/internal/logx"
	"hush/server/internal/push"
	"hush/server/internal/store"
)

const hubShards = 64

type epochEntry struct {
	n  uint64
	at time.Time
}

type shard struct {
	mu    sync.RWMutex
	conns map[string]*conn
	// epoch 在每次 Kick / Disconnect 时换成一个新的全局递增值。新连接在鉴权前记下 epoch，登记时若已变化，说明其间发生了
	// 重新登录或退出登录（旧 token 已作废），拒绝登记，避免它挤掉新设备或在 Kick 之后才上线而逃过踢出。
	// 条目在一段时间后清理（见 PruneEpochs），不会随用户数无限增长。
	epoch map[string]epochEntry
}

// Hub 维护 user_id → 当前连接（单设备：同一用户只保留最新连接）。连接表按用户 id 分片，查在线状态不争用全局锁。
type Hub struct {
	store  *store.Store
	pusher push.Pusher

	seed     maphash.Seed
	shards   [hubShards]shard
	online   atomic.Int64
	epochSeq atomic.Uint64

	// 连接数上限（0 表示不限）
	maxConns      atomic.Int64
	maxConnsPerIP atomic.Int64
	ipMu          sync.Mutex
	perIP         map[string]int
	admitted      atomic.Int64

	pushCh      chan pushReq
	pushDropped atomic.Int64

	closing atomic.Bool
	serving sync.WaitGroup

	// 提交钩子里复用：同一条共享正文（群消息）只拼一次帧
	lastPayload *byte
	lastFrame   []byte

	beforeRegister func(userID string) // 仅供测试：校验 token 之后、登记连接之前调用，用于构造与重新登录交错的时序
}

type pushReq struct {
	userID, reason string
	flush          chan struct{}
}

// NewHub 创建 Hub，并注册为 store 的提交钩子：任何事务提交后，其写入的队列项由 Hub 直接交给在线连接。
func NewHub(st *store.Store, p push.Pusher) *Hub {
	if p == nil {
		p = push.LogPusher{}
	}
	h := &Hub{store: st, pusher: p, seed: maphash.MakeSeed(), perIP: map[string]int{}, pushCh: make(chan pushReq, 4096)}
	for i := range h.shards {
		h.shards[i].conns = map[string]*conn{}
		h.shards[i].epoch = map[string]epochEntry{}
	}
	go h.pushLoop()
	st.SetCommitHook(h.onCommit)
	return h
}

func (h *Hub) shard(userID string) *shard {
	return &h.shards[maphash.String(h.seed, userID)%hubShards]
}

// pushLoop 在独立协程里调用 Pusher：推送服务（FCM/APNs）再慢也不会拖住写协程或请求。
func (h *Hub) pushLoop() {
	for req := range h.pushCh {
		if req.flush != nil {
			close(req.flush)
			continue
		}
		h.pusher.Push(req.userID, req.reason)
	}
}

func (h *Hub) push(userID, reason string) {
	select {
	case h.pushCh <- pushReq{userID: userID, reason: reason}:
	default:
		// 推送队列满了（推送服务卡住）：丢弃这次唤醒，消息本身已在队列里，用户上线后照常收到
		if n := h.pushDropped.Add(1); n == 1 || n%1000 == 0 {
			logx.Warnf("[push] 推送队列已满，已丢弃 %d 次离线唤醒", n)
		}
	}
}

// FlushPushes 等待此前排队的离线推送都已交给 Pusher（测试用）。
func (h *Hub) FlushPushes() {
	done := make(chan struct{})
	h.pushCh <- pushReq{flush: done}
	<-done
}

// staleAfter：在线连接超过这么久没有收到客户端的任何帧（客户端每 25 秒 ping 一次），就可能是半开连接，
// 此时 urgent 信封在照常下发的同时也触发离线推送。
const staleAfter = 40 * time.Second

// onCommit 在队列项所在事务提交之后、由写协程按提交顺序调用：在线则把队列项直接放进连接的发送缓冲
// （不用再回数据库读一遍），离线且 urgent 的信封触发推送（每个用户一次）。
// 队列项在此之前已经落库，进程在这里之前崩溃也不会丢：客户端重连后作为积压收到。
func (h *Hub) onCommit(items []store.QueueInsert) {
	var pushed map[string]bool
	now := time.Now().UnixNano()
	for i := range items {
		it := &items[i]
		c := h.get(it.UserID)
		needPush := it.Urgent && it.Kind == store.KindEnvelope
		if c != nil {
			c.offer(liveItem{seq: it.Seq, id: it.ID, kind: it.Kind, frame: h.frameFor(it)})
			if !needPush || now-c.lastRead.Load() < int64(staleAfter) {
				continue
			}
		}
		if needPush && !pushed[it.UserID] {
			if pushed == nil {
				pushed = map[string]bool{}
			}
			pushed[it.UserID] = true
			h.push(it.UserID, it.Kind)
		}
	}
}

// frameFor 拼出队列项的 WS 帧；共享正文（同一条群消息发给很多人）只拼一次。只在写协程里调用。
func (h *Hub) frameFor(it *store.QueueInsert) []byte {
	if !it.Shared || len(it.Payload) == 0 {
		return Frame(it.Kind, it.Payload)
	}
	if p := &it.Payload[0]; p != h.lastPayload {
		h.lastPayload, h.lastFrame = p, Frame(it.Kind, it.Payload)
	}
	return h.lastFrame
}

// Item 是一条要投递给某用户的队列项。
type Item struct {
	UserID      string
	ID          string
	Kind        string // store.KindEnvelope | store.KindEvent
	Payload     []byte
	Urgent      bool
	CoalesceKey string // 仅事件：见 store.QueueInsert.CoalesceKey
	SenderID    string // 仅信封：发送方（子配额）
	Shared      bool   // 正文只存一份（同一个 ID 发给多人）
}

// Inserts 把 Item 转成 store.QueueInsert。
func Inserts(items []Item) []store.QueueInsert {
	ins := make([]store.QueueInsert, len(items))
	for i, it := range items {
		ins[i] = store.QueueInsert{UserID: it.UserID, ID: it.ID, Kind: it.Kind, Payload: it.Payload, CoalesceKey: it.CoalesceKey,
			Urgent: it.Urgent, SenderID: it.SenderID, Shared: it.Shared}
	}
	return ins
}

// Deliver 在独立事务中持久化队列项（信封），提交后交给在线连接；离线且 urgent 的触发 Pusher。
// 信封受 lim 约束（配额检查与写入在同一事务内），返回值与 items 一一对应，表示是否已接收。
func (h *Hub) Deliver(ctx context.Context, items []Item, lim store.QueueLimits) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	return h.store.Enqueue(ctx, Inserts(items), lim)
}

// Event 构造事件负载：{"id","type",...fields}。
func Event(typ string, fields map[string]any) map[string]any {
	ev := map[string]any{"type": typ}
	for k, v := range fields {
		ev[k] = v
	}
	return ev
}

// EmitOpt 调整 Emit 的合并方式。
type EmitOpt struct {
	// Keep > 0：同键事件只保留最新的 Keep 条，不考虑与信封的相对顺序（见 store.QueueInsert.CoalesceKeep）。
	Keep int
}

// Emit 在业务事务 t 内把同一事件（同一 id）写入多个用户的队列（outbox，接口 v2 S-D）：
// 事件与业务写入一起提交或一起回滚，提交之后才交给在线连接（见 onCommit）。事件不触发推送，也不受信封配额限制。
// coalesceKey 非空时，各接收方队列里同键的旧事件会被这条替换（规则见 store.QueueInsert.CoalesceKey），防止反复操作灌满他人队列。
// 发给多人时正文只存一份。
func Emit(t *store.Tx, userIDs []string, typ, coalesceKey string, fields map[string]any, opts ...EmitOpt) error {
	if len(userIDs) == 0 {
		return nil
	}
	id := store.NewID()
	ev := Event(typ, fields)
	ev["id"] = id
	payload, err := MarshalNoEscape(ev)
	if err != nil {
		return fmt.Errorf("序列化事件 %s: %w", typ, err)
	}
	keep := 0
	if len(opts) > 0 {
		keep = opts[0].Keep
	}
	items := make([]store.QueueInsert, 0, len(userIDs))
	seen := make(map[string]bool, len(userIDs))
	for _, uid := range userIDs {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		items = append(items, store.QueueInsert{UserID: uid, ID: id, Kind: store.KindEvent, Payload: payload, CoalesceKey: coalesceKey, CoalesceKeep: keep})
	}
	if len(items) > 1 {
		for i := range items {
			items[i].Shared = true
		}
	}
	if _, err := t.Enqueue(items, store.QueueLimits{}); err != nil {
		return fmt.Errorf("写入事件 %s: %w", typ, err)
	}
	logx.Debugf("[event] %s -> %d 个用户", typ, len(items))
	return nil
}

func (h *Hub) get(userID string) *conn {
	s := h.shard(userID)
	s.mu.RLock()
	c := s.conns[userID]
	s.mu.RUnlock()
	return c
}

// OnlineCount 返回当前在线连接数（管理后台仪表盘 / 监控）。
func (h *Hub) OnlineCount() int { return int(h.online.Load()) }

// IsOnline 判断用户当前是否有在线连接。
func (h *Hub) IsOnline(userID string) bool { return h.get(userID) != nil }

// currentEpoch 返回用户当前的 epoch。
func (h *Hub) currentEpoch(userID string) uint64 {
	s := h.shard(userID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.epoch[userID].n
}

// register 在 epoch 未变化时登记新连接，返回被挤掉的旧连接（可能为 nil）；epoch 已变化返回 ok=false。
func (h *Hub) register(c *conn, epoch uint64) (old *conn, ok bool) {
	s := h.shard(c.userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch[c.userID].n != epoch {
		return nil, false
	}
	old = s.conns[c.userID]
	s.conns[c.userID] = c
	if old == nil {
		h.online.Add(1)
	}
	return old, true
}

// unregister 仅当 c 仍是当前连接时移除。
func (h *Hub) unregister(c *conn) {
	s := h.shard(c.userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[c.userID] == c {
		delete(s.conns, c.userID)
		h.online.Add(-1)
	}
}

// take 取出并移除用户当前连接，同时更换 epoch（此后仍在建立中的连接不能再登记）。
func (h *Hub) take(userID string) *conn {
	s := h.shard(userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch[userID] = epochEntry{n: h.epochSeq.Add(1), at: time.Now()}
	c := s.conns[userID]
	if c != nil {
		delete(s.conns, userID)
		h.online.Add(-1)
	}
	return c
}

// epochTTL：epoch 条目只需要覆盖「鉴权 → 登记」这一小段时间（WebSocket 握手，至多几十秒）。
const epochTTL = 10 * time.Minute

// PruneEpochs 删除早于 epochTTL 的 epoch 条目，返回删除个数（定时清理调用；否则每个被踢过的用户都永久占一项）。
func (h *Hub) PruneEpochs(now time.Time) int {
	n := 0
	for i := range h.shards {
		s := &h.shards[i]
		s.mu.Lock()
		for uid, e := range s.epoch {
			if now.Sub(e.at) > epochTTL {
				delete(s.epoch, uid)
				n++
			}
		}
		s.mu.Unlock()
	}
	return n
}

// Kick 向用户当前连接发送一帧（不持久化）后关闭，用于 logged_out。
func (h *Hub) Kick(userID string, frame any, code int, reason string) {
	c := h.take(userID)
	if c == nil {
		return
	}
	data, _ := MarshalNoEscape(frame)
	c.finish(data, code, reason)
	logx.Infof("[ws] 踢下线 user=%s reason=%s", userID, reason)
}

// Disconnect 关闭用户当前连接（如退出登录）。
func (h *Hub) Disconnect(userID string, code int, reason string) {
	if c := h.take(userID); c != nil {
		c.finish(nil, code, reason)
	}
}

// ---------- 连接数上限 ----------

var (
	// ErrTooManyConns 总连接数已达上限。
	ErrTooManyConns = errors.New("too many connections")
	// ErrTooManyConnsPerIP 该 IP 的连接数已达上限。
	ErrTooManyConnsPerIP = errors.New("too many connections from this address")
	// ErrShuttingDown 服务端正在停机，不再接受新连接。
	ErrShuttingDown = errors.New("server shutting down")
)

// SetLimits 设置最大连接数与每 IP 连接数（0 表示不限）。
func (h *Hub) SetLimits(maxConns, maxPerIP int) {
	h.maxConns.Store(int64(maxConns))
	h.maxConnsPerIP.Store(int64(maxPerIP))
}

// Acquire 为来自 ip 的一条新连接占一个名额；成功后必须调用 Release。
func (h *Hub) Acquire(ip string) error {
	if h.closing.Load() {
		return ErrShuttingDown
	}
	if max := h.maxConns.Load(); max > 0 && h.admitted.Load() >= max {
		return ErrTooManyConns
	}
	h.ipMu.Lock()
	if max := h.maxConnsPerIP.Load(); max > 0 && int64(h.perIP[ip]) >= max {
		h.ipMu.Unlock()
		return ErrTooManyConnsPerIP
	}
	h.perIP[ip]++
	h.ipMu.Unlock()
	h.admitted.Add(1)
	return nil
}

// Release 归还 Acquire 占的名额。
func (h *Hub) Release(ip string) {
	h.ipMu.Lock()
	if h.perIP[ip]--; h.perIP[ip] <= 0 {
		delete(h.perIP, ip)
	}
	h.ipMu.Unlock()
	h.admitted.Add(-1)
}

// ---------- 优雅停机 ----------

// CloseGoingAway 是停机时发给客户端的关闭码（RFC 6455：1001 Going Away）。客户端按普通断线处理，稍后重连。
const CloseGoingAway = 1001

// Shutdown 停止接受新连接，给每条在线连接发关闭帧（1001），并等它们的读写协程退出（或 ctx 到期）。
// 返回时不再有连接协程访问数据库，之后可以安全关库。未 ack 的数据都在队列里，客户端重连后照常收到。
func (h *Hub) Shutdown(ctx context.Context) error {
	h.closing.Store(true)
	for i := range h.shards {
		s := &h.shards[i]
		s.mu.Lock()
		conns := make([]*conn, 0, len(s.conns))
		for _, c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		for _, c := range conns {
			c.finish(nil, CloseGoingAway, "server shutting down")
		}
	}
	done := make(chan struct{})
	go func() { h.serving.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// 超时：强行断开剩下的连接，再等它们退出
		for i := range h.shards {
			s := &h.shards[i]
			s.mu.Lock()
			for _, c := range s.conns {
				c.cancel()
			}
			s.mu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}
}
