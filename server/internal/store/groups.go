package store

import (
	"context"
	"database/sql"
	"errors"
)

// 群成员角色。
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Member 是群成员（契约 3.6）。
type Member struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
	JoinedAt int64  `json:"joined_at"`
}

// Group 是群信息（契约 3.6）。
type Group struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	AvatarID     *string  `json:"avatar_id"`
	Announcement string   `json:"announcement"`
	OwnerID      string   `json:"owner_id"`
	Members      []Member `json:"members"`
	UpdatedAt    int64    `json:"updated_at"`
}

// CreateGroup 建群（独立事务）。
func (s *Store) CreateGroup(ctx context.Context, ownerID, name string, memberIDs []string) (Group, error) {
	return update(s, ctx, func(t *Tx) (Group, error) { return t.CreateGroup(ownerID, name, memberIDs) })
}

// CreateGroup 建群：owner 为创建者，其余为普通成员。返回建好的群（含成员）。
func (t *Tx) CreateGroup(ownerID, name string, memberIDs []string) (Group, error) {
	ctx, tx := t.ctx, t.tx
	now := NowMillis()
	gid := NewID()
	if _, err := tx.ExecContext(ctx, "INSERT INTO groups (id, name, owner_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		gid, name, ownerID, now, now); err != nil {
		return Group{}, err
	}
	if err := insertMember(ctx, tx, gid, ownerID, RoleOwner, now); err != nil {
		return Group{}, err
	}
	for _, m := range memberIDs {
		if err := insertMember(ctx, tx, gid, m, RoleMember, now); err != nil {
			return Group{}, err
		}
	}
	return t.GetGroup(gid)
}

func insertMember(ctx context.Context, ex execer, gid, uid, role string, now int64) error {
	_, err := ex.ExecContext(ctx, "INSERT INTO group_members (group_id, user_id, role, joined_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING",
		gid, uid, role, now)
	return err
}

// GetGroup 查询群及成员。
func (s *Store) GetGroup(ctx context.Context, id string) (Group, error) {
	return getGroup(ctx, s.db, id)
}

// GetGroup 在事务内查询群及成员。
func (t *Tx) GetGroup(id string) (Group, error) { return getGroup(t.ctx, t.tx, id) }

func getGroup(ctx context.Context, ex execer, id string) (Group, error) {
	gs, err := loadGroups(ctx, ex, "", "g.id = ?", id)
	if err != nil {
		return Group{}, err
	}
	if len(gs) == 0 {
		return Group{}, ErrNotFound
	}
	return gs[0], nil
}

// UserGroups 返回用户所在的全部群（按加入时间），固定两次查询。
func (s *Store) UserGroups(ctx context.Context, userID string) ([]Group, error) {
	return loadGroups(ctx, s.db, userID, "g.id IN (SELECT group_id FROM group_members WHERE user_id = ?)", userID)
}

// loadGroups 按条件读取群及其成员：一次查群、一次查全部成员。结果按 viewer 的入群时间排序。
func loadGroups(ctx context.Context, ex execer, viewer, where string, args ...any) ([]Group, error) {
	rows, err := ex.QueryContext(ctx, `SELECT g.id, g.name, g.avatar_id, g.announcement, g.owner_id, g.updated_at FROM groups g
		LEFT JOIN group_members me ON me.group_id = g.id AND me.user_id = ?
		WHERE `+where+` ORDER BY me.joined_at, g.rowid`, append([]any{viewer}, args...)...)
	if err != nil {
		return nil, err
	}
	out := []Group{}
	index := map[string]int{}
	for rows.Next() {
		var g Group
		var avatar sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &avatar, &g.Announcement, &g.OwnerID, &g.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if avatar.Valid {
			g.AvatarID = &avatar.String
		}
		g.Members = []Member{}
		index[g.ID] = len(out)
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]string, len(out))
	for i, g := range out {
		ids[i] = g.ID
	}
	mrows, err := ex.QueryContext(ctx, "SELECT group_id, user_id, role, joined_at FROM group_members WHERE group_id IN ("+placeholders(len(ids))+
		") ORDER BY joined_at, rowid", toArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var gid string
		var m Member
		if err := mrows.Scan(&gid, &m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		g := &out[index[gid]]
		g.Members = append(g.Members, m)
	}
	return out, mrows.Err()
}

// GroupRecipients 返回群消息的接收方：除发送者外、且没有拉黑发送者的成员（一次查询）。
func (s *Store) GroupRecipients(ctx context.Context, gid, sender string) ([]string, error) {
	return s.queryIDs(ctx, `SELECT gm.user_id FROM group_members gm WHERE gm.group_id = ? AND gm.user_id <> ?
		AND NOT EXISTS (SELECT 1 FROM blocks b WHERE b.user_id = gm.user_id AND b.blocked_id = ?)
		ORDER BY gm.joined_at, gm.rowid`, gid, sender, sender)
}

// MemberRole 返回成员角色；非成员返回 ErrNotFound。
func (s *Store) MemberRole(ctx context.Context, gid, uid string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, "SELECT role FROM group_members WHERE group_id = ? AND user_id = ?", gid, uid).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// GroupUpdate 描述可选字段更新；AvatarID 指向空串表示清除。
type GroupUpdate struct {
	Name         *string
	AvatarID     *string
	Announcement *string
}

// UpdateGroup 修改群资料（独立事务）。
func (s *Store) UpdateGroup(ctx context.Context, gid string, up GroupUpdate) error {
	return s.Update(ctx, func(t *Tx) error { return t.UpdateGroup(gid, up) })
}

// UpdateGroup 在事务内修改群资料。
func (t *Tx) UpdateGroup(gid string, up GroupUpdate) error {
	ctx, tx := t.ctx, t.tx
	if up.Name != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET name = ? WHERE id = ?", *up.Name, gid); err != nil {
			return err
		}
	}
	if up.Announcement != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET announcement = ? WHERE id = ?", *up.Announcement, gid); err != nil {
			return err
		}
	}
	if up.AvatarID != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET avatar_id = ? WHERE id = ?", nullIfEmpty(*up.AvatarID), gid); err != nil {
			return err
		}
	}
	return touchGroup(ctx, tx, gid)
}

func touchGroup(ctx context.Context, ex execer, gid string) error {
	_, err := ex.ExecContext(ctx, "UPDATE groups SET updated_at = ? WHERE id = ?", NowMillis(), gid)
	return err
}

// ErrTooManyMembers 加人后群成员数会超过上限。
var ErrTooManyMembers = errors.New("too many members")

// AddMembers 添加普通成员（已是成员的忽略）。人数上限在同一事务内检查（maxMembers<=0 表示不限），
// 两个管理员并发加人也不会超过上限；超限返回 ErrTooManyMembers 且不添加任何人。
func (s *Store) AddMembers(ctx context.Context, gid string, ids []string, maxMembers int) error {
	return s.Update(ctx, func(t *Tx) error { return t.AddMembers(gid, ids, maxMembers) })
}

// AddMembers 见 Store.AddMembers，在调用方的事务内执行。
func (t *Tx) AddMembers(gid string, ids []string, maxMembers int) error {
	ctx, tx := t.ctx, t.tx
	now := NowMillis()
	if maxMembers > 0 && len(ids) > 0 {
		var current int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM group_members WHERE group_id = ?", gid).Scan(&current); err != nil {
			return err
		}
		// ids 已由调用方去重；其中已是成员的不占新名额
		var already int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM group_members WHERE group_id = ? AND user_id IN ("+placeholders(len(ids))+")",
			append([]any{gid}, toArgs(ids)...)...).Scan(&already); err != nil {
			return err
		}
		if current+len(ids)-already > maxMembers {
			return ErrTooManyMembers
		}
	}
	for _, id := range ids {
		if err := insertMember(ctx, tx, gid, id, RoleMember, now); err != nil {
			return err
		}
	}
	return touchGroup(ctx, tx, gid)
}

// RemoveMember 移除成员。若移除的是群主，则群主转让给最早加入的管理员，否则最早加入的成员；
// 群内无人时删除群。返回 dissolved=true 表示群已删除。
func (s *Store) RemoveMember(ctx context.Context, gid, uid string) (dissolved bool, err error) {
	return update(s, ctx, func(t *Tx) (bool, error) { return t.RemoveMember(gid, uid) })
}

// RemoveMember 见 Store.RemoveMember，在调用方的事务内执行。
func (t *Tx) RemoveMember(gid, uid string) (dissolved bool, err error) {
	ctx, tx := t.ctx, t.tx
	err = func() error {
		var role string
		if err := tx.QueryRowContext(ctx, "SELECT role FROM group_members WHERE group_id = ? AND user_id = ?", gid, uid).Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM group_members WHERE group_id = ? AND user_id = ?", gid, uid); err != nil {
			return err
		}
		if role == RoleOwner {
			var next string
			err := tx.QueryRowContext(ctx, `SELECT user_id FROM group_members WHERE group_id = ?
				ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END, joined_at, rowid LIMIT 1`, gid).Scan(&next)
			if errors.Is(err, sql.ErrNoRows) {
				dissolved = true
				_, err = tx.ExecContext(ctx, "DELETE FROM groups WHERE id = ?", gid)
				return err
			}
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE group_members SET role = 'owner' WHERE group_id = ? AND user_id = ?", gid, next); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE groups SET owner_id = ? WHERE id = ?", next, gid); err != nil {
				return err
			}
		}
		return touchGroup(ctx, tx, gid)
	}()
	return dissolved, err
}

// SetRole 修改成员角色（admin/member）。
func (s *Store) SetRole(ctx context.Context, gid, uid, role string) error {
	return s.Update(ctx, func(t *Tx) error { return t.SetRole(gid, uid, role) })
}

// SetRole 在事务内修改成员角色。
func (t *Tx) SetRole(gid, uid, role string) error {
	if _, err := t.tx.ExecContext(t.ctx, "UPDATE group_members SET role = ? WHERE group_id = ? AND user_id = ?", role, gid, uid); err != nil {
		return err
	}
	return touchGroup(t.ctx, t.tx, gid)
}
