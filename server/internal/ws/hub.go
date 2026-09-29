// Package ws 负责在线连接管理与投递：信封/事件按接收方持久化进队列，在线则经 WebSocket 推送。
package ws

import (
	"context"
	"fmt"
	"log"
	"sync"

	"hush/server/internal/push"
	"hush/server/internal/store"
)

// Hub 维护 user_id → 当前连接（单设备：同一用户只保留最新连接）。
type Hub struct {
	store  *store.Store
	pusher push.Pusher

	mu    sync.Mutex
	conns map[string]*conn
	// epoch 在每次 Kick / Disconnect 时递增。新连接在鉴权前记下 epoch，登记时若已变化，说明其间发生了
	// 重新登录或退出登录（旧 token 已作废），拒绝登记，避免它挤掉新设备或在 Kick 之后才上线而逃过踢出。
	epoch map[string]uint64

	beforeRegister func(userID string) // 仅供测试：校验 token 之后、登记连接之前调用，用于构造与重新登录交错的时序
}

// NewHub 创建 Hub，并注册为 store 的提交钩子：任何事务提交后，其写入的队列项由 Hub 通知在线连接。
func NewHub(st *store.Store, p push.Pusher) *Hub {
	if p == nil {
		p = push.LogPusher{}
	}
	h := &Hub{store: st, pusher: p, conns: map[string]*conn{}, epoch: map[string]uint64{}}
	st.SetCommitHook(h.onCommit)
	return h
}

// onCommit 在队列项所在事务提交之后调用：在线则唤醒连接去拉取，离线且 urgent 的信封触发推送（每个用户一次）。
// 队列项在此之前已经落库，进程在这里之前崩溃也不会丢：客户端重连后作为积压收到。
func (h *Hub) onCommit(items []store.QueueInsert) {
	pushed := map[string]bool{}
	for _, it := range items {
		if h.notify(it.UserID) {
			continue
		}
		if it.Urgent && !pushed[it.UserID] {
			pushed[it.UserID] = true
			h.pusher.Push(it.UserID, it.Kind)
		}
	}
}

// Item 是一条要投递给某用户的队列项。
type Item struct {
	UserID      string
	ID          string
	Kind        string // store.KindEnvelope | store.KindEvent
	Payload     []byte
	Urgent      bool
	CoalesceKey string // 仅事件：见 store.QueueInsert.CoalesceKey
}

// Deliver 在独立事务中持久化队列项（信封），提交后通知在线连接；离线且 urgent 的触发 Pusher。
// 信封受 lim 约束（配额检查与写入在同一事务内），返回值与 items 一一对应，表示是否已接收。
func (h *Hub) Deliver(ctx context.Context, items []Item, lim store.QueueLimits) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ins := make([]store.QueueInsert, len(items))
	for i, it := range items {
		ins[i] = store.QueueInsert{UserID: it.UserID, ID: it.ID, Kind: it.Kind, Payload: it.Payload, CoalesceKey: it.CoalesceKey, Urgent: it.Urgent}
	}
	return h.store.Enqueue(ctx, ins, lim)
}

// Event 构造事件负载：{"id","type",...fields}。
func Event(typ string, fields map[string]any) map[string]any {
	ev := map[string]any{"type": typ}
	for k, v := range fields {
		ev[k] = v
	}
	return ev
}

// Emit 在业务事务 t 内把同一事件（同一 id）写入多个用户的队列（outbox，接口 v2 S-D）：
// 事件与业务写入一起提交或一起回滚，提交之后才通知在线连接（见 onCommit）。事件不触发推送，也不受队列配额限制。
// coalesceKey 非空时，各接收方队列里同键的旧事件（其后没有信封时）会被这条替换，防止反复操作灌满他人队列。
func Emit(t *store.Tx, userIDs []string, typ, coalesceKey string, fields map[string]any) error {
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
	items := make([]store.QueueInsert, 0, len(userIDs))
	seen := map[string]bool{}
	for _, uid := range userIDs {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		items = append(items, store.QueueInsert{UserID: uid, ID: id, Kind: store.KindEvent, Payload: payload, CoalesceKey: coalesceKey})
	}
	if _, err := t.Enqueue(items, store.QueueLimits{}); err != nil {
		return fmt.Errorf("写入事件 %s: %w", typ, err)
	}
	log.Printf("[event] %s -> %d 个用户", typ, len(items))
	return nil
}

// notify 唤醒用户连接去拉取新队列项；返回是否在线。
func (h *Hub) notify(userID string) bool {
	h.mu.Lock()
	c := h.conns[userID]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	c.wake()
	return true
}

// currentEpoch 返回用户当前的 epoch。
func (h *Hub) currentEpoch(userID string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.epoch[userID]
}

// register 在 epoch 未变化时登记新连接，返回被挤掉的旧连接（可能为 nil）；epoch 已变化返回 ok=false。
func (h *Hub) register(c *conn, epoch uint64) (old *conn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.epoch[c.userID] != epoch {
		return nil, false
	}
	old = h.conns[c.userID]
	h.conns[c.userID] = c
	return old, true
}

// unregister 仅当 c 仍是当前连接时移除。
func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.userID] == c {
		delete(h.conns, c.userID)
	}
}

// take 取出并移除用户当前连接，同时递增 epoch（此后仍在建立中的连接不能再登记）。
func (h *Hub) take(userID string) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epoch[userID]++
	c := h.conns[userID]
	delete(h.conns, userID)
	return c
}

// Kick 向用户当前连接发送一帧（不持久化）后关闭，用于 logged_out。
func (h *Hub) Kick(userID string, frame any, code int, reason string) {
	c := h.take(userID)
	if c == nil {
		return
	}
	data, _ := MarshalNoEscape(frame)
	c.finish(data, code, reason)
	log.Printf("[ws] 踢下线 user=%s reason=%s", userID, reason)
}

// Disconnect 关闭用户当前连接（如退出登录）。
func (h *Hub) Disconnect(userID string, code int, reason string) {
	if c := h.take(userID); c != nil {
		c.finish(nil, code, reason)
	}
}
