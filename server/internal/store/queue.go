package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
	// CoalesceKey 非空时（仅用于事件），入队时替换该用户队列里同键的旧事件，防止反复操作灌满他人队列：
	//   - 旧事件之后没有信封：直接删除，新事件排到队尾；
	//   - 旧事件之后已有信封：它不能挪到后面（例如 group_updated 必须先于新成员的消息），于是只保留最早的那一条并把它的内容
	//     原地换成最新状态（换一个新 id），其余的删除。这样同一个键在队列里最多两条：最早的位置上一条、队尾一条，
	//     都是最新状态——与信封交替操作也无法让事件无限堆积。
	CoalesceKey string
	// CoalesceKeep > 0 时改用「只保留最新的 CoalesceKeep 条（含这一条）」的规则，不考虑与信封的相对顺序。
	// 用于与消息顺序无关的事件（朋友圈评论通知、「有消息被拒」）。
	CoalesceKeep int
	// SenderID 是信封的发送方，用于每（发送方, 接收方）子配额。事件留空。
	SenderID string
	// Shared 为 true 时正文只存一份（envelopes 表，按 ID 引用），用于同一条内容发给很多人的场合
	// （群消息、发给多人的事件）：同一个 ID 的各条必须带同样的 Payload。无人引用时由触发器删除。
	Shared bool
	// Urgent 不落库，只随提交钩子传给 ws.Hub：接收方离线时是否触发推送（仅信封）。
	Urgent bool
	// Seq 由 Enqueue 在写入后填上（队列里的顺序号），随提交钩子交给 ws.Hub。
	Seq int64
}

// QueueLimits 是接收方队列配额（只约束信封，事件不占用也不受限）。零值表示不限。
type QueueLimits struct {
	MaxItems int64 // 每个接收方队列里的信封总条数
	MaxBytes int64 // 每个接收方队列里的信封总字节
	// 每（发送方, 接收方）子配额：单个发送方最多在某个接收方的队列里占这么多，灌满了也只影响他自己。
	SenderMaxItems int64
	SenderMaxBytes int64
}

func (l QueueLimits) any() bool {
	return l.MaxItems > 0 || l.MaxBytes > 0 || l.SenderMaxItems > 0 || l.SenderMaxBytes > 0
}

// Verdict 是一条队列项的入队结果。
type Verdict uint8

const (
	Accepted       Verdict = iota // 已接收（含重复写入被忽略的情况）
	RejectedFull                  // 接收方队列已满
	RejectedSender                // 该发送方在接收方队列里的子配额已满
)

// OK 表示已接收。
func (v Verdict) OK() bool { return v == Accepted }

// QueueItem 是已持久化的队列项。
type QueueItem struct {
	Seq     int64
	ID      string
	Kind    string
	Payload json.RawMessage // nil 表示正文已丢失（不应发生），调用方应直接删除该项
}

// Enqueue 在一个独立事务中写入多条队列项（见 Tx.Enqueue），提交后通知在线连接。
func (s *Store) Enqueue(ctx context.Context, items []QueueInsert, lim QueueLimits) ([]bool, error) {
	return update(s, ctx, func(t *Tx) ([]bool, error) { return t.Enqueue(items, lim) })
}

// Enqueue 在本事务中写入多条队列项，返回值与 items 一一对应，表示是否已接收（见 EnqueueVerdicts）。
func (t *Tx) Enqueue(items []QueueInsert, lim QueueLimits) ([]bool, error) {
	vs, err := t.EnqueueVerdicts(items, lim)
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(vs))
	for i, v := range vs {
		out[i] = v.OK()
	}
	return out, nil
}

type usage struct{ items, bytes int64 }

const insertChunk = 50 // 一条 INSERT 最多带多少行

// enqueuer 是一次 EnqueueVerdicts 调用的工作状态。
type enqueuer struct {
	t     *Tx
	items []QueueInsert
	lim   QueueLimits
	now   int64

	total  map[string]*usage // 接收方 → 信封用量（含本次调用里已接受的）
	sender map[string]*usage // 接收方 + "\x00" + 发送方 → 用量

	pending    []int           // 待写入的 items 下标
	sharedDone map[string]bool // 已写入 envelopes 的 id
	existing   map[string][]coalesceRow
	preloaded  bool // existing 已对全体接收方预查
}

type coalesceRow struct {
	seq    int64
	pinned bool // 后面有信封
}

// EnqueueVerdicts 在本事务中写入多条队列项（与业务写入一起提交）。同一 (user_id, id) 重复写入会被忽略（视为已接收）。
// 信封受 lim 约束：接收方队列或该发送方的子配额将超限时拒收该条，不丢弃已有数据。
// 配额检查与写入在同一事务内完成，并发请求不会突破上限。返回值与 items 一一对应。
//
// 写入按批进行（多行 INSERT）：500 人群的一条消息是十来条语句，而不是上千条。
func (t *Tx) EnqueueVerdicts(items []QueueInsert, lim QueueLimits) ([]Verdict, error) {
	out := make([]Verdict, len(items))
	if len(items) == 0 {
		return out, nil
	}
	e := &enqueuer{t: t, items: items, lim: lim, now: NowMillis()}
	if err := e.preloadUsage(); err != nil {
		return nil, err
	}
	if err := e.preloadCoalesce(); err != nil {
		return nil, err
	}
	for i := range items {
		it := &items[i]
		size := int64(len(it.Payload))
		if it.Kind == KindEnvelope && lim.any() {
			v, err := e.checkQuota(it, size)
			if err != nil {
				return nil, err
			}
			if v != Accepted {
				out[i] = v
				continue
			}
		}
		if it.CoalesceKey != "" && it.Kind == KindEvent {
			if err := e.coalesce(it); err != nil {
				return nil, err
			}
		}
		e.pending = append(e.pending, i)
		if len(e.pending) >= insertChunk {
			if err := e.flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := e.flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// preloadUsage 一次查出全部信封接收方的用量（以及发送方唯一时的子配额用量）。
func (e *enqueuer) preloadUsage() error {
	if !e.lim.any() {
		return nil
	}
	e.total, e.sender = map[string]*usage{}, map[string]*usage{}
	var users []string
	senderID, oneSender := "", true
	seen := map[string]bool{}
	for i := range e.items {
		it := &e.items[i]
		if it.Kind != KindEnvelope {
			continue
		}
		if !seen[it.UserID] {
			seen[it.UserID] = true
			users = append(users, it.UserID)
		}
		if senderID == "" {
			senderID = it.SenderID
		} else if it.SenderID != senderID {
			oneSender = false
		}
	}
	if len(users) < 2 { // 单个接收方：用到时再查（一条语句）
		return nil
	}
	withSender := oneSender && senderID != "" && (e.lim.SenderMaxItems > 0 || e.lim.SenderMaxBytes > 0)
	list, err := json.Marshal(users)
	if err != nil {
		return err
	}
	ctx, tx := e.t.ctx, e.t.tx
	rows, err := tx.QueryContext(ctx, `SELECT u.value, COALESCE(qu.env_items, 0), COALESCE(qu.env_bytes, 0) FROM json_each(?) u
		LEFT JOIN queue_usage qu ON qu.user_id = u.value`, string(list))
	if err != nil {
		return err
	}
	for rows.Next() {
		var uid string
		u := &usage{}
		if err := rows.Scan(&uid, &u.items, &u.bytes); err != nil {
			rows.Close()
			return err
		}
		e.total[uid] = u
		if withSender {
			e.sender[uid+"\x00"+senderID] = &usage{}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !withSender {
		return nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT q.user_id, COUNT(*), COALESCE(SUM(q.size), 0) FROM queue q
		WHERE q.sender_id = ? AND q.user_id IN (SELECT value FROM json_each(?)) GROUP BY q.user_id`, senderID, string(list))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		u := &usage{}
		if err := rows.Scan(&uid, &u.items, &u.bytes); err != nil {
			return err
		}
		e.sender[uid+"\x00"+senderID] = u
	}
	return rows.Err()
}

// checkQuota 判断一条信封能否入队，能则把它计入本次调用的用量。
func (e *enqueuer) checkQuota(it *QueueInsert, size int64) (Verdict, error) {
	ctx, tx := e.t.ctx, e.t.tx
	lim := e.lim
	perSender := it.SenderID != "" && (lim.SenderMaxItems > 0 || lim.SenderMaxBytes > 0)
	tot := e.total[it.UserID]
	var snd *usage
	skey := ""
	if perSender {
		skey = it.UserID + "\x00" + it.SenderID
		snd = e.sender[skey]
	}
	if tot == nil || (perSender && snd == nil) {
		if e.total == nil {
			e.total, e.sender = map[string]*usage{}, map[string]*usage{}
		}
		nt, ns := &usage{}, &usage{}
		var err error
		if perSender {
			err = tx.QueryRowContext(ctx, `SELECT
				COALESCE((SELECT env_items FROM queue_usage WHERE user_id = ?1), 0),
				COALESCE((SELECT env_bytes FROM queue_usage WHERE user_id = ?1), 0),
				(SELECT COUNT(*) FROM queue WHERE user_id = ?1 AND sender_id = ?2),
				(SELECT COALESCE(SUM(size), 0) FROM queue WHERE user_id = ?1 AND sender_id = ?2)`, it.UserID, it.SenderID).
				Scan(&nt.items, &nt.bytes, &ns.items, &ns.bytes)
		} else {
			nt.items, nt.bytes, err = envUsage(ctx, tx, it.UserID)
		}
		if err != nil {
			return 0, err
		}
		if tot == nil {
			tot = nt
			e.total[it.UserID] = tot
		}
		if perSender && snd == nil {
			snd = ns
			e.sender[skey] = snd
		}
	}
	if (lim.MaxItems > 0 && tot.items+1 > lim.MaxItems) || (lim.MaxBytes > 0 && tot.bytes+size > lim.MaxBytes) {
		return RejectedFull, nil
	}
	if snd != nil && ((lim.SenderMaxItems > 0 && snd.items+1 > lim.SenderMaxItems) || (lim.SenderMaxBytes > 0 && snd.bytes+size > lim.SenderMaxBytes)) {
		return RejectedSender, nil
	}
	tot.items++
	tot.bytes += size
	if snd != nil {
		snd.items++
		snd.bytes += size
	}
	return Accepted, nil
}

const coalesceRowsSQL = `SELECT q.user_id, q.seq,
	EXISTS(SELECT 1 FROM queue e WHERE e.user_id = q.user_id AND e.seq > q.seq AND e.kind = 'envelope')
	FROM queue q WHERE q.coalesce_key = ? AND `

// preloadCoalesce 在「同一个事件发给很多人」的情形下（全部是同键事件、接收方互不相同），一次查出各接收方队列里的同键旧事件。
func (e *enqueuer) preloadCoalesce() error {
	if len(e.items) < 2 {
		return nil
	}
	key := e.items[0].CoalesceKey
	if key == "" {
		return nil
	}
	users := make([]string, 0, len(e.items))
	seen := make(map[string]bool, len(e.items))
	for i := range e.items {
		it := &e.items[i]
		if it.Kind != KindEvent || it.CoalesceKey != key || it.CoalesceKeep != 0 || seen[it.UserID] {
			return nil
		}
		seen[it.UserID] = true
		users = append(users, it.UserID)
	}
	list, err := json.Marshal(users)
	if err != nil {
		return err
	}
	rows, err := e.t.tx.QueryContext(e.t.ctx, coalesceRowsSQL+"q.user_id IN (SELECT value FROM json_each(?)) ORDER BY q.seq", key, string(list))
	if err != nil {
		return err
	}
	defer rows.Close()
	e.existing = map[string][]coalesceRow{}
	for rows.Next() {
		var uid string
		var r coalesceRow
		if err := rows.Scan(&uid, &r.seq, &r.pinned); err != nil {
			return err
		}
		e.existing[uid] = append(e.existing[uid], r)
	}
	e.preloaded = true
	return rows.Err()
}

// coalesce 在写入事件 it 之前处理该用户队列里同键的旧事件（规则见 QueueInsert.CoalesceKey）。
func (e *enqueuer) coalesce(it *QueueInsert) error {
	ctx, tx := e.t.ctx, e.t.tx
	if it.CoalesceKeep > 0 {
		if err := e.flush(); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE user_id = ?1 AND coalesce_key = ?2 AND id <> ?3 AND seq NOT IN (
			SELECT seq FROM queue WHERE user_id = ?1 AND coalesce_key = ?2 AND id <> ?3 ORDER BY seq DESC LIMIT ?4)`,
			it.UserID, it.CoalesceKey, it.ID, it.CoalesceKeep-1)
		return err
	}
	var rows []coalesceRow
	if e.preloaded {
		rows = e.existing[it.UserID]
	} else {
		if err := e.flush(); err != nil {
			return err
		}
		rs, err := tx.QueryContext(ctx, coalesceRowsSQL+"q.user_id = ? AND q.id <> ? ORDER BY q.seq", it.CoalesceKey, it.UserID, it.ID)
		if err != nil {
			return err
		}
		for rs.Next() {
			var uid string
			var r coalesceRow
			if err := rs.Scan(&uid, &r.seq, &r.pinned); err != nil {
				rs.Close()
				return err
			}
			rows = append(rows, r)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	var pinned int64 = -1
	var del []any
	for _, r := range rows {
		if r.pinned && pinned < 0 {
			pinned = r.seq // 最早的、后面有信封的那一条：原地换成最新状态
			continue
		}
		del = append(del, r.seq)
	}
	if len(del) > 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM queue WHERE seq IN ("+placeholders(len(del))+")", del...); err != nil {
			return err
		}
	}
	if pinned < 0 {
		return nil
	}
	// 原地替换：删掉旧行，用同一个 seq 写入最新状态（新的 id，正文内联）。事件里的 "id" 字段一并换掉；
	// 找不到就不动这条（保持旧行为：保留旧事件）。
	newID := NewID()
	oldTok, newTok := []byte(`"id":"`+it.ID+`"`), []byte(`"id":"`+newID+`"`)
	if !bytes.Contains(it.Payload, oldTok) {
		return nil
	}
	payload := bytes.Replace(it.Payload, oldTok, newTok, 1)
	if _, err := tx.ExecContext(ctx, "DELETE FROM queue WHERE seq = ?", pinned); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO queue (seq, user_id, id, kind, payload, created_at, size, coalesce_key, shared, sender_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)`, pinned, it.UserID, newID, it.Kind, string(payload), e.now, len(payload), it.CoalesceKey)
	return err
}

// flush 把 pending 里的队列项用一条多行 INSERT 写入，并把新写入的（带 seq）记进事务的 queued。
func (e *enqueuer) flush() error {
	if len(e.pending) == 0 {
		return nil
	}
	ctx, tx := e.t.ctx, e.t.tx
	idx := e.pending
	e.pending = e.pending[:0]
	var sb strings.Builder
	sb.WriteString("INSERT INTO queue (user_id, id, kind, payload, created_at, size, coalesce_key, shared, sender_id) VALUES ")
	args := make([]any, 0, len(idx)*9)
	byKey := make(map[string]int, len(idx))
	for n, i := range idx {
		it := &e.items[i]
		if n > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("(?,?,?,?,?,?,?,?,?)")
		payload, shared := "", 0
		if it.Shared {
			shared = 1
			if !e.sharedDone[it.ID] {
				if e.sharedDone == nil {
					e.sharedDone = map[string]bool{}
				}
				e.sharedDone[it.ID] = true
				if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO envelopes (id, payload, created_at) VALUES (?, ?, ?)", it.ID, string(it.Payload), e.now); err != nil {
					return err
				}
			}
		} else {
			payload = string(it.Payload)
		}
		var key, sender any
		if it.CoalesceKey != "" && it.Kind == KindEvent {
			key = it.CoalesceKey
		}
		if it.SenderID != "" {
			sender = it.SenderID
		}
		args = append(args, it.UserID, it.ID, it.Kind, payload, e.now, len(it.Payload), key, shared, sender)
		byKey[it.UserID+"\x00"+it.ID] = i
	}
	sb.WriteString(" ON CONFLICT(user_id, id) DO NOTHING RETURNING seq, user_id, id")
	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var uid, id string
		if err := rows.Scan(&seq, &uid, &id); err != nil {
			return err
		}
		if i, ok := byKey[uid+"\x00"+id]; ok {
			e.items[i].Seq = seq
			e.t.queued = append(e.t.queued, e.items[i])
		}
	}
	return rows.Err()
}

const queueSelect = `SELECT q.seq, q.id, q.kind, q.size,
	CASE WHEN q.shared = 1 THEN (SELECT e.payload FROM envelopes e WHERE e.id = q.id) ELSE q.payload END
	FROM queue q WHERE q.user_id = ? AND q.seq > ? ORDER BY q.seq LIMIT ?`

// QueueAfter 按顺序返回 seq > after 的队列项：最多 limit 条，且累计负载超过 maxBytes 后不再继续读取
// （至少返回 1 条；maxBytes<=0 表示不限字节）。
func (s *Store) QueueAfter(ctx context.Context, userID string, after int64, limit int, maxBytes int64) ([]QueueItem, error) {
	rows, err := s.rd.QueryContext(ctx, queueSelect, userID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	var total int64
	for rows.Next() {
		var it QueueItem
		var size int64
		var payload sql.NullString
		if err := rows.Scan(&it.Seq, &it.ID, &it.Kind, &size, &payload); err != nil {
			return nil, err
		}
		if maxBytes > 0 && len(out) > 0 && total+size > maxBytes {
			break
		}
		total += size
		if payload.Valid {
			it.Payload = json.RawMessage(payload.String)
		}
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
// 检查与删除在同一事务内（同一条语句），旧设备在重新登录生效之后发来的 ack 不会删掉发给新设备的数据。
// 多个连接的 ack 由写协程合并进同一次提交。
func (s *Store) AckAs(ctx context.Context, userID, tokenHash string, ids []string) (int64, error) {
	var n int64
	err := s.withTx(ctx, func(tx dbtx) error {
		if len(ids) > 0 {
			args := append([]any{userID}, toArgs(ids)...)
			args = append(args, tokenHash, userID)
			res, err := tx.ExecContext(ctx, "DELETE FROM queue WHERE user_id = ? AND id IN ("+placeholders(len(ids))+
				") AND EXISTS (SELECT 1 FROM tokens WHERE token_hash = ? AND user_id = ?)", args...)
			if err != nil {
				return err
			}
			if n, err = res.RowsAffected(); err != nil || n > 0 {
				return err
			}
		}
		// 一条也没删：区分「token 已作废」与「这些 id 本来就不在队列里」
		if ok, err := exists(ctx, tx, "SELECT 1 FROM tokens WHERE token_hash = ? AND user_id = ?", tokenHash, userID); err != nil {
			return err
		} else if !ok {
			return ErrNotFound
		}
		return nil
	})
	return n, err
}

// QueueUsage 返回某接收方队列中的条数与负载总字节数（信封与事件合计；由触发器维护的计数器，O(1)）。
func (s *Store) QueueUsage(ctx context.Context, userID string) (items int64, bytes int64, err error) {
	err = s.rd.QueryRowContext(ctx, "SELECT items, bytes FROM queue_usage WHERE user_id = ?", userID).Scan(&items, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return
}

// EnvelopeUsage 返回某接收方队列中信封的条数与字节数（配额只看这个：事件不占用信封配额）。
func (s *Store) EnvelopeUsage(ctx context.Context, userID string) (items int64, bytes int64, err error) {
	return envUsage(ctx, s.rd, userID)
}

func envUsage(ctx context.Context, ex execer, userID string) (items int64, bytes int64, err error) {
	err = ex.QueryRowContext(ctx, "SELECT env_items, env_bytes FROM queue_usage WHERE user_id = ?", userID).Scan(&items, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return
}

const pruneBatch = 1000

// PruneQueue 删除入队时间早于 before（毫秒）的队列项，返回删除条数。
// 分批删除（每批一个小事务），批与批之间其他写入可以插进来：一次清理几十万行也不会让全库停顿。
func (s *Store) PruneQueue(ctx context.Context, before int64) (int64, error) {
	return s.deleteBatched(ctx, "DELETE FROM queue WHERE seq IN (SELECT seq FROM queue WHERE created_at < ? LIMIT ?)", before)
}

// deleteBatched 反复执行带 LIMIT 子查询的删除语句（最后一个参数是批大小），直到删不到东西。
func (s *Store) deleteBatched(ctx context.Context, q string, args ...any) (int64, error) {
	var total int64
	args = append(args, pruneBatch)
	for {
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}

// PruneOrphanEnvelopes 删除没有任何队列项引用的共享正文（正常情况下触发器已经删掉，这里兜底）。
func (s *Store) PruneOrphanEnvelopes(ctx context.Context, before int64) (int64, error) {
	return s.deleteBatched(ctx, `DELETE FROM envelopes WHERE id IN (SELECT e.id FROM envelopes e WHERE e.created_at < ?
		AND NOT EXISTS (SELECT 1 FROM queue q WHERE q.id = e.id AND q.shared = 1) LIMIT ?)`, before)
}
