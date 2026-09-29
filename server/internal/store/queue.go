package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// 队列项类型，同时也是 WS 帧的 t 字段。
const (
	KindEnvelope = "envelope"
	KindEvent    = "event"
)

// QueueInsert 是一条待写入的队列项。
type QueueInsert struct {
	UserID  string
	ID      string
	Kind    string
	Payload []byte
	// CoalesceKey 非空时（仅用于事件），入队前删除该用户队列里同键的旧事件——前提是旧事件之后没有信封，
	// 这样新事件挪到队尾不会改变它与信封的相对顺序（例如 group_updated 必须先于新成员的消息）。
	CoalesceKey string
	// Urgent 不落库，只随提交钩子传给 ws.Hub：接收方离线时是否触发推送（仅信封）。
	Urgent bool
}

// QueueLimits 是接收方队列配额（只约束信封，事件不受限）。零值表示不限。
type QueueLimits struct {
	MaxItems int64
	MaxBytes int64
}

// QueueItem 是已持久化的队列项。
type QueueItem struct {
	Seq     int64
	ID      string
	Kind    string
	Payload json.RawMessage
}

// Enqueue 在一个独立事务中写入多条队列项（见 Tx.Enqueue），提交后通知在线连接。
func (s *Store) Enqueue(ctx context.Context, items []QueueInsert, lim QueueLimits) ([]bool, error) {
	return update(s, ctx, func(t *Tx) ([]bool, error) { return t.Enqueue(items, lim) })
}

// Enqueue 在本事务中写入多条队列项（与业务写入一起提交）。同一 (user_id, id) 重复写入会被忽略（视为已接收）。
// 信封受 lim 约束：接收方队列（条数或字节）将超限时拒收该条，不丢弃已有数据。
// 配额检查与写入在同一事务内完成，并发请求不会突破上限。返回值与 items 一一对应，表示是否已接收。
func (t *Tx) Enqueue(items []QueueInsert, lim QueueLimits) ([]bool, error) {
	ctx, tx := t.ctx, t.tx
	accepted := make([]bool, len(items))
	now := NowMillis()
	for i, it := range items {
		size := int64(len(it.Payload))
		if it.Kind == KindEnvelope && (lim.MaxItems > 0 || lim.MaxBytes > 0) {
			n, b, err := queueUsage(ctx, tx, it.UserID)
			if err != nil {
				return nil, err
			}
			if (lim.MaxItems > 0 && n+1 > lim.MaxItems) || (lim.MaxBytes > 0 && b+size > lim.MaxBytes) {
				continue
			}
		}
		var key any
		if it.CoalesceKey != "" && it.Kind == KindEvent {
			key = it.CoalesceKey
			if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE user_id = ? AND coalesce_key = ? AND id <> ?
				AND NOT EXISTS (SELECT 1 FROM queue e WHERE e.user_id = queue.user_id AND e.seq > queue.seq AND e.kind = 'envelope')`,
				it.UserID, it.CoalesceKey, it.ID); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO queue (user_id, id, kind, payload, created_at, size, coalesce_key) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, id) DO NOTHING`, it.UserID, it.ID, it.Kind, string(it.Payload), now, size, key); err != nil {
			return nil, err
		}
		accepted[i] = true
		t.queued = append(t.queued, it)
	}
	return accepted, nil
}

// QueueAfter 按顺序返回 seq > after 的队列项：最多 limit 条，且累计负载超过 maxBytes 后不再继续读取
// （至少返回 1 条；maxBytes<=0 表示不限字节）。
func (s *Store) QueueAfter(ctx context.Context, userID string, after int64, limit int, maxBytes int64) ([]QueueItem, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT seq, id, kind, size, payload FROM queue WHERE user_id = ? AND seq > ? ORDER BY seq LIMIT ?", userID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	var total int64
	for rows.Next() {
		var it QueueItem
		var size int64
		var payload string
		if err := rows.Scan(&it.Seq, &it.ID, &it.Kind, &size, &payload); err != nil {
			return nil, err
		}
		if maxBytes > 0 && len(out) > 0 && total+size > maxBytes {
			break
		}
		total += size
		it.Payload = json.RawMessage(payload)
		out = append(out, it)
	}
	return out, rows.Err()
}

// Ack 删除该用户已确认的队列项，返回删除条数。
func (s *Store) Ack(ctx context.Context, userID string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]any{userID}, toArgs(ids)...)
	res, err := s.db.ExecContext(ctx, "DELETE FROM queue WHERE user_id = ? AND id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AckAs 以某个 token 的身份确认队列项：token 已作废（例如用户在别处重新登录）时返回 ErrNotFound 且不删除任何数据。
// 检查与删除在同一事务内，旧设备在重新登录生效之后发来的 ack 不会删掉发给新设备的数据。
func (s *Store) AckAs(ctx context.Context, userID, tokenHash string, ids []string) (int64, error) {
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if ok, err := exists(ctx, tx, "SELECT 1 FROM tokens WHERE token_hash = ? AND user_id = ?", tokenHash, userID); err != nil {
			return err
		} else if !ok {
			return ErrNotFound
		}
		if len(ids) == 0 {
			return nil
		}
		args := append([]any{userID}, toArgs(ids)...)
		res, err := tx.ExecContext(ctx, "DELETE FROM queue WHERE user_id = ? AND id IN ("+placeholders(len(ids))+")", args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// QueueUsage 返回某接收方队列中的条数与负载总字节数（由触发器维护的计数器，O(1)）。
func (s *Store) QueueUsage(ctx context.Context, userID string) (items int64, bytes int64, err error) {
	return queueUsage(ctx, s.db, userID)
}

func queueUsage(ctx context.Context, ex execer, userID string) (items int64, bytes int64, err error) {
	err = ex.QueryRowContext(ctx, "SELECT items, bytes FROM queue_usage WHERE user_id = ?", userID).Scan(&items, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return
}

// PruneQueue 删除入队时间早于 before（毫秒）的队列项，返回删除条数。
func (s *Store) PruneQueue(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM queue WHERE created_at < ?", before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
