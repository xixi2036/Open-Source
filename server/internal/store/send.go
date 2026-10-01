package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SendDedupTTL 是发送幂等键的保留时间：同一个 (发送方, 目标, client_id) 在这段时间内重复发送，返回第一次的结果而不再投递。
const SendDedupTTL = 24 * time.Hour

// DedupKey 是发送幂等键里由请求决定的部分。Target：单聊为接收方 id，群消息为群 id。
type DedupKey struct{ Target, ClientID string }

// DedupHit 是首次发送时分配的信封 id 与时间。
type DedupHit struct {
	ID       string
	ServerTS int64
}

func dedupHash(sender string, k DedupKey) []byte {
	h := sha256.New()
	h.Write([]byte(sender))
	h.Write([]byte{0})
	h.Write([]byte(k.Target))
	h.Write([]byte{0})
	h.Write([]byte(k.ClientID))
	return h.Sum(nil)[:16]
}

// 信封 id 是 UUID：按 16 字节存，读出来再还原成字符串。
func idToBlob(id string) []byte {
	if u, err := uuid.Parse(id); err == nil && len(id) == 36 {
		return u[:]
	}
	return []byte(id)
}

func blobToID(b []byte) string {
	if len(b) == 16 {
		var u uuid.UUID
		copy(u[:], b)
		return u.String()
	}
	return string(b)
}

// SendDedupLookup 查出 keys 里已经发送过（且未过期）的那些。
func (t *Tx) SendDedupLookup(sender string, keys []DedupKey, now int64) (map[DedupKey]DedupHit, error) {
	out := map[DedupKey]DedupHit{}
	cut := now - SendDedupTTL.Milliseconds()
	byHash := make(map[string]DedupKey, len(keys))
	for len(keys) > 0 {
		chunk := keys[:min(len(keys), 100)]
		keys = keys[len(chunk):]
		args := make([]any, 0, len(chunk)+1)
		for _, k := range chunk {
			h := dedupHash(sender, k)
			byHash[string(h)] = k
			args = append(args, h)
		}
		args = append(args, cut)
		rows, err := t.tx.QueryContext(t.ctx, "SELECT key, id, server_ts FROM send_dedup WHERE key IN ("+placeholders(len(chunk))+") AND server_ts >= ?", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, id []byte
			var h DedupHit
			if err := rows.Scan(&key, &id, &h.ServerTS); err != nil {
				rows.Close()
				return nil, err
			}
			h.ID = blobToID(id)
			out[byHash[string(key)]] = h
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DedupRecord 是一条要记下的幂等键。
type DedupRecord struct {
	DedupKey
	DedupHit
}

// SendDedupRecord 记下已接受的发送（过期的旧记录被覆盖）。
func (t *Tx) SendDedupRecord(sender string, recs []DedupRecord) error {
	for len(recs) > 0 {
		chunk := recs[:min(len(recs), 100)]
		recs = recs[len(chunk):]
		var sb strings.Builder
		sb.WriteString("INSERT OR REPLACE INTO send_dedup (key, id, server_ts) VALUES ")
		args := make([]any, 0, len(chunk)*3)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?)")
			args = append(args, dedupHash(sender, r.DedupKey), idToBlob(r.ID), r.ServerTS)
		}
		if _, err := t.tx.ExecContext(t.ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// PruneSendDedup 删除 before（毫秒）之前的幂等键。
func (s *Store) PruneSendDedup(ctx context.Context, before int64) (int64, error) {
	return s.deleteBatched(ctx, "DELETE FROM send_dedup WHERE key IN (SELECT key FROM send_dedup WHERE server_ts < ? LIMIT ?)", before)
}

// PruneNotifications 删除 before 之前的朋友圈互动通知。
func (s *Store) PruneNotifications(ctx context.Context, before int64) (int64, error) {
	return s.deleteBatched(ctx, "DELETE FROM moment_notifications WHERE rowid IN (SELECT rowid FROM moment_notifications WHERE created_at < ? LIMIT ?)", before)
}

// PruneHandledRequests 删除 before 之前处理完（已接受 / 已拒绝）的好友申请。待处理的不动。
func (s *Store) PruneHandledRequests(ctx context.Context, before int64) (int64, error) {
	return s.deleteBatched(ctx, "DELETE FROM friend_requests WHERE rowid IN (SELECT rowid FROM friend_requests WHERE status <> 'pending' AND updated_at < ? LIMIT ?)", before)
}

// ErrNotMember 操作者不是该群成员。
var ErrNotMember = errors.New("not a member")

// GroupSendRecipients 在写事务内确认 sender 此刻仍是群成员，并返回群消息的接收方：除发送者外、且没有拉黑发送者的成员。
// 群不存在返回 ErrNotFound，不是成员返回 ErrNotMember。与入队在同一事务里：刚被移出群的人发不出去。
func (t *Tx) GroupSendRecipients(gid, sender string) ([]string, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT gm.user_id,
		EXISTS (SELECT 1 FROM blocks b WHERE b.user_id = gm.user_id AND b.blocked_id = ?)
		FROM group_members gm WHERE gm.group_id = ? ORDER BY gm.joined_at, gm.rowid`, sender, gid)
	if err != nil {
		return nil, err
	}
	var out []string
	member, any := false, false
	for rows.Next() {
		var uid string
		var blocked bool
		if err := rows.Scan(&uid, &blocked); err != nil {
			rows.Close()
			return nil, err
		}
		any = true
		if uid == sender {
			member = true
			continue
		}
		if !blocked {
			out = append(out, uid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if member {
		return out, nil
	}
	if any {
		return nil, ErrNotMember
	}
	if ok, err := exists(t.ctx, t.tx, "SELECT 1 FROM groups WHERE id = ?", gid); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotFound
	}
	return nil, ErrNotMember
}

// MemberRole 在事务内返回成员角色；非成员返回 ErrNotFound。
func (t *Tx) MemberRole(gid, uid string) (string, error) {
	var role string
	err := t.tx.QueryRowContext(t.ctx, "SELECT role FROM group_members WHERE group_id = ? AND user_id = ?", gid, uid).Scan(&role)
	if isNoRows(err) {
		return "", ErrNotFound
	}
	return role, err
}
