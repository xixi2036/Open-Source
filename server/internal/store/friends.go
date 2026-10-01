package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// 好友申请状态。
const (
	RequestPending  = "pending"
	RequestAccepted = "accepted"
	RequestRejected = "rejected"
)

// FriendRequest 是好友申请的原始记录（不含资料）。
type FriendRequest struct {
	ID        string
	FromID    string
	ToID      string
	Message   string
	Status    string
	CreatedAt int64
}

const requestCols = "id, from_id, to_id, message, status, created_at"

func scanRequest(sc interface{ Scan(...any) error }) (FriendRequest, error) {
	var r FriendRequest
	err := sc.Scan(&r.ID, &r.FromID, &r.ToID, &r.Message, &r.Status, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// AreFriends 判断双方是否为好友。
func (s *Store) AreFriends(ctx context.Context, a, b string) (bool, error) {
	return areFriends(ctx, s.db, a, b)
}

// AreFriends 在事务内判断双方是否为好友。
func (t *Tx) AreFriends(a, b string) (bool, error) { return areFriends(t.ctx, t.tx, a, b) }

func areFriends(ctx context.Context, ex execer, a, b string) (bool, error) {
	return exists(ctx, ex, "SELECT 1 FROM friendships WHERE user_id = ? AND friend_id = ?", a, b)
}

// FriendsAmong 返回 ids 中是 userID 好友的那些（一次查询）。
func (s *Store) FriendsAmong(ctx context.Context, userID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := s.queryIDs(ctx, "SELECT friend_id FROM friendships WHERE user_id = ? AND friend_id IN ("+placeholders(len(ids))+")",
		append([]any{userID}, toArgs(ids)...)...)
	for _, id := range found {
		out[id] = true
	}
	return out, err
}

// BlockedAmong 返回 ids 中与 userID 存在拉黑关系（任一方拉黑另一方）的那些。
func (s *Store) BlockedAmong(ctx context.Context, userID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	found, err := s.queryIDs(ctx, `SELECT j.value FROM json_each(?1) j WHERE EXISTS (SELECT 1 FROM blocks b
		WHERE (b.user_id = ?2 AND b.blocked_id = j.value) OR (b.user_id = j.value AND b.blocked_id = ?2))`, string(list), userID)
	for _, id := range found {
		out[id] = true
	}
	return out, err
}

// BlockedEither 在事务内判断 a 与 b 之间是否存在拉黑关系。
func (t *Tx) BlockedEither(a, b string) (bool, error) {
	return exists(t.ctx, t.tx, blockedEitherSQL, a, b, b, a)
}

// Friends 返回好友资料列表（互为好友，带邮箱）。
func (s *Store) Friends(ctx context.Context, userID string) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+prefixed("u", userCols)+` FROM friendships f JOIN users u ON u.id = f.friend_id
		WHERE f.user_id = ? ORDER BY f.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Profile{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u.WithEmail())
	}
	return out, rows.Err()
}

// Relation 是 a 与 b 的关系快照（一次查询得到）。
type Relation struct {
	Exists     bool // b 存在
	Blocked    bool // 任一方拉黑了另一方
	Friends    bool
	ShareGroup bool
}

// Relation 一次查询返回 a 与 b 的关系：b 是否存在、是否拉黑、是否好友、是否共同在群。
func (s *Store) Relation(ctx context.Context, a, b string) (Relation, error) {
	rels, err := s.Relations(ctx, a, []string{b})
	return rels[b], err
}

// relationCols 是以 ?1 为 a、以表达式 b 为对方的关系判断列。已经是好友时不再查共同群（结论不受影响，省一次连接查询）。
func relationCols(b string) string {
	return `EXISTS(SELECT 1 FROM users WHERE id = ` + b + `),
		EXISTS(SELECT 1 FROM blocks WHERE (user_id = ?1 AND blocked_id = ` + b + `) OR (user_id = ` + b + ` AND blocked_id = ?1)),
		EXISTS(SELECT 1 FROM friendships WHERE user_id = ?1 AND friend_id = ` + b + `),
		CASE WHEN EXISTS(SELECT 1 FROM friendships WHERE user_id = ?1 AND friend_id = ` + b + `) THEN 0 ELSE
			EXISTS(SELECT 1 FROM group_members g1 JOIN group_members g2 ON g1.group_id = g2.group_id WHERE g1.user_id = ?1 AND g2.user_id = ` + b + `) END`
}

var (
	relationOneSQL  = "SELECT " + relationCols("?2")
	relationManySQL = "SELECT t.value, " + relationCols("t.value") + " FROM (SELECT DISTINCT value FROM json_each(?2)) t"
)

// Relations 用一次集合查询返回 a 与 ids 中每个用户的关系（Olm 批量发送的关系校验，接口 v2 S-F）。
// ids 以 JSON 数组作为单个参数传入（json_each 展开），不受 SQL 变量个数限制。结果以 id 为键，重复 id 只算一次。
// ShareGroup 只在双方不是好友时才查（已是好友时恒为 false）。
func (s *Store) Relations(ctx context.Context, a string, ids []string) (map[string]Relation, error) {
	out := make(map[string]Relation, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	gen := s.rel.gen.Load() // 查库之前记下：期间关系有变动的话，这次的结果不进缓存
	if len(ids) == 1 {      // 单聊的常见情形：不必展开 JSON
		r, hit := s.rel.get(a, ids[0], gen)
		if !hit {
			if err := s.rd.QueryRowContext(ctx, relationOneSQL, a, ids[0]).Scan(&r.Exists, &r.Blocked, &r.Friends, &r.ShareGroup); err != nil {
				return nil, err
			}
			s.rel.put(a, ids[0], r, gen)
		}
		out[ids[0]] = r
		return out, nil
	}
	var miss []string
	for _, id := range ids {
		if r, hit := s.rel.get(a, id, gen); hit {
			out[id] = r
		} else {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	list, err := json.Marshal(miss)
	if err != nil {
		return nil, err
	}
	rows, err := s.rd.QueryContext(ctx, relationManySQL, a, string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r Relation
		if err := rows.Scan(&id, &r.Exists, &r.Blocked, &r.Friends, &r.ShareGroup); err != nil {
			return nil, err
		}
		out[id] = r
		s.rel.put(a, id, r, gen)
	}
	return out, rows.Err()
}

// relationCache 缓存「a 可以给 b 发消息」这个结论（只缓存允许的结果：存在、没有拉黑、是好友或同群）。
// 每条单聊消息都要做这个判断，而关系很少变化。任何可能改变关系的写入（好友、拉黑、群成员、删除用户 / 群）提交后，
// 整个缓存立即失效（代数 +1，见 txConn.relDirty）；另外每条记录最多用 relationCacheTTL，作为兜底。
type relationCache struct {
	m   sync.Map // a + "\x00" + b → relEntry
	n   atomic.Int64
	gen atomic.Uint64
}

type relEntry struct {
	r   Relation
	gen uint64
	at  int64
}

const (
	relationCacheTTL = 30 * time.Second
	relationCacheMax = 500_000
)

func (c *relationCache) invalidate() { c.gen.Add(1) }

func (c *relationCache) get(a, b string, gen uint64) (Relation, bool) {
	v, ok := c.m.Load(a + "\x00" + b)
	if !ok {
		return Relation{}, false
	}
	e := v.(relEntry)
	if e.gen != gen || time.Now().UnixMilli()-e.at > relationCacheTTL.Milliseconds() {
		return Relation{}, false
	}
	return e.r, true
}

func (c *relationCache) put(a, b string, r Relation, gen uint64) {
	if !r.Exists || r.Blocked || !(r.Friends || r.ShareGroup) {
		return
	}
	if c.n.Load() >= relationCacheMax {
		c.m.Range(func(k, _ any) bool { c.m.Delete(k); return true })
		c.n.Store(0)
	}
	if _, loaded := c.m.Swap(a+"\x00"+b, relEntry{r: r, gen: gen, at: time.Now().UnixMilli()}); !loaded {
		c.n.Add(1)
	}
}

// blockedEitherSQL 需要参数 (a, b, b, a)。
const blockedEitherSQL = "SELECT 1 FROM blocks WHERE (user_id = ? AND blocked_id = ?) OR (user_id = ? AND blocked_id = ?)"

// pendingRequest 查询 from→to 的待处理申请。
func pendingRequest(ctx context.Context, ex execer, from, to string) (FriendRequest, error) {
	return scanRequest(ex.QueryRowContext(ctx, "SELECT "+requestCols+` FROM friend_requests
		WHERE from_id = ? AND to_id = ? AND status = 'pending' ORDER BY created_at DESC LIMIT 1`, from, to))
}

// ErrBlocked 双方之间存在拉黑关系。
var ErrBlocked = errors.New("blocked")

// SendRequestResult 是 SendFriendRequest 的结果。
type SendRequestResult struct {
	Request       FriendRequest
	BecameFriends bool // 对方已向我发出待处理申请：直接成为好友，Request 为对方那条（status=accepted）
}

// SendFriendRequest 在一个事务内完成好友申请的全部检查与写入：
// 对方不存在 → ErrNotFound；任一方拉黑 → ErrBlocked；已是好友 → ErrConflict；
// 对方已有发给我的待处理申请 → 直接成为好友；否则新建申请，已有 from→to 的待处理申请则更新留言与时间并复用。
func (s *Store) SendFriendRequest(ctx context.Context, from, to, message string) (SendRequestResult, error) {
	return update(s, ctx, func(t *Tx) (SendRequestResult, error) { return t.SendFriendRequest(from, to, message) })
}

// SendFriendRequest 见 Store.SendFriendRequest，在调用方的事务内执行。
func (t *Tx) SendFriendRequest(from, to, message string) (SendRequestResult, error) {
	ctx, tx := t.ctx, t.tx
	var res SendRequestResult
	now := NowMillis()
	err := func() error {
		if ok, err := exists(ctx, tx, "SELECT 1 FROM users WHERE id = ?", to); err != nil {
			return err
		} else if !ok {
			return ErrNotFound
		}
		if ok, err := exists(ctx, tx, blockedEitherSQL, from, to, to, from); err != nil {
			return err
		} else if ok {
			return ErrBlocked
		}
		if ok, err := exists(ctx, tx, "SELECT 1 FROM friendships WHERE user_id = ? AND friend_id = ?", from, to); err != nil {
			return err
		} else if ok {
			return ErrConflict
		}
		reverse, err := pendingRequest(ctx, tx, to, from)
		if err == nil {
			if err := makeFriends(ctx, tx, from, to, now); err != nil {
				return err
			}
			reverse.Status = RequestAccepted
			res = SendRequestResult{Request: reverse, BecameFriends: true}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if r, err := pendingRequest(ctx, tx, from, to); err == nil {
			if _, err := tx.ExecContext(ctx, "UPDATE friend_requests SET message = ?, created_at = ?, updated_at = ? WHERE id = ?", message, now, now, r.ID); err != nil {
				return err
			}
			r.Message, r.CreatedAt = message, now
			res.Request = r
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		r := FriendRequest{ID: NewID(), FromID: from, ToID: to, Message: message, Status: RequestPending, CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO friend_requests (id, from_id, to_id, message, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'pending', ?, ?)`, r.ID, from, to, message, now, now); err != nil {
			return err
		}
		res.Request = r
		return nil
	}()
	return res, err
}

// AcceptFriendRequest 在一个事务内接受发给 uid 的待处理申请并建立好友关系。
// 申请不存在或不是发给 uid 的 → ErrNotFound；已处理过 → ErrConflict；存在拉黑关系 → ErrBlocked。
// 并发接受同一条申请时只有一次成功（另一次得到 ErrConflict），因此 friend_accepted 只发一次。
func (s *Store) AcceptFriendRequest(ctx context.Context, id, uid string) (FriendRequest, error) {
	return update(s, ctx, func(t *Tx) (FriendRequest, error) { return t.AcceptFriendRequest(id, uid) })
}

// AcceptFriendRequest 见 Store.AcceptFriendRequest，在调用方的事务内执行。
func (t *Tx) AcceptFriendRequest(id, uid string) (FriendRequest, error) {
	ctx, tx := t.ctx, t.tx
	var fr FriendRequest
	err := func() error {
		var err error
		fr, err = scanRequest(tx.QueryRowContext(ctx, "SELECT "+requestCols+" FROM friend_requests WHERE id = ?", id))
		if err != nil {
			return err
		}
		if fr.ToID != uid {
			return ErrNotFound
		}
		if fr.Status != RequestPending {
			return ErrConflict
		}
		if ok, err := exists(ctx, tx, blockedEitherSQL, uid, fr.FromID, fr.FromID, uid); err != nil {
			return err
		} else if ok {
			return ErrBlocked
		}
		return makeFriends(ctx, tx, uid, fr.FromID, NowMillis())
	}()
	return fr, err
}

// RejectFriendRequest 拒绝发给 uid 的待处理申请：不存在或不是发给 uid 的 → ErrNotFound；已处理过 → ErrConflict。
func (s *Store) RejectFriendRequest(ctx context.Context, id, uid string) (FriendRequest, error) {
	var fr FriendRequest
	err := s.withTx(ctx, func(tx dbtx) error {
		var err error
		fr, err = scanRequest(tx.QueryRowContext(ctx, "SELECT "+requestCols+" FROM friend_requests WHERE id = ?", id))
		if err != nil {
			return err
		}
		if fr.ToID != uid {
			return ErrNotFound
		}
		if fr.Status != RequestPending {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, "UPDATE friend_requests SET status = ?, updated_at = ? WHERE id = ?", RequestRejected, NowMillis(), id)
		return err
	})
	return fr, err
}

// makeFriends 建立双向好友关系，并把两人之间所有待处理申请标记为 accepted。
func makeFriends(ctx context.Context, tx dbtx, a, b string, now int64) error {
	for _, p := range [][2]string{{a, b}, {b, a}} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO friendships (user_id, friend_id, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING", p[0], p[1], now); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE friend_requests SET status = 'accepted', updated_at = ?
		WHERE status = 'pending' AND ((from_id = ? AND to_id = ?) OR (from_id = ? AND to_id = ?))`, now, a, b, b, a)
	return err
}

// RemoveFriend 双向删除好友关系，返回之前是否为好友。
func (s *Store) RemoveFriend(ctx context.Context, a, b string) (bool, error) {
	return update(s, ctx, func(t *Tx) (bool, error) { return t.RemoveFriend(a, b) })
}

// RemoveFriend 在事务内双向删除好友关系，返回之前是否为好友。
func (t *Tx) RemoveFriend(a, b string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, "DELETE FROM friendships WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", a, b, b, a)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PendingRequests 返回用户收到与发出的待处理申请。
func (s *Store) PendingRequests(ctx context.Context, userID string) (incoming, outgoing []FriendRequest, err error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+requestCols+` FROM friend_requests
		WHERE status = 'pending' AND (to_id = ? OR from_id = ?) ORDER BY created_at DESC`, userID, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, nil, err
		}
		if r.ToID == userID {
			incoming = append(incoming, r)
		} else {
			outgoing = append(outgoing, r)
		}
	}
	return incoming, outgoing, rows.Err()
}

// Block 拉黑。
func (s *Store) Block(ctx context.Context, userID, target string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO blocks (user_id, blocked_id, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING", userID, target, NowMillis())
	return err
}

// Unblock 取消拉黑。
func (s *Store) Unblock(ctx context.Context, userID, target string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM blocks WHERE user_id = ? AND blocked_id = ?", userID, target)
	return err
}

// Blocked 返回被拉黑的用户 id 列表。
func (s *Store) Blocked(ctx context.Context, userID string) ([]string, error) {
	ids, err := s.queryIDs(ctx, "SELECT blocked_id FROM blocks WHERE user_id = ? ORDER BY created_at", userID)
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}
