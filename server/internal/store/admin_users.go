package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrDisabled 账号已被管理员停用。
var ErrDisabled = errors.New("account disabled")

// AdminUser 是管理后台看到的用户记录（只含服务端本来就持有的资料，不含任何消息内容）。
type AdminUser struct {
	ID               string  `json:"id"`
	Email            string  `json:"email"`
	Name             string  `json:"name"`
	About            string  `json:"about"`
	AvatarID         *string `json:"avatar_id"`
	Platform         string  `json:"platform"`
	DeviceName       string  `json:"device_name"`
	DeviceGeneration int64   `json:"device_generation"`
	CreatedAt        int64   `json:"created_at"`
	LastActiveAt     int64   `json:"last_active_at"`
	DisabledAt       int64   `json:"disabled_at"`
}

const adminUserCols = "id, email, name, about, avatar_id, platform, device_name, device_generation, created_at, last_active_at, disabled_at"

func scanAdminUser(sc interface{ Scan(...any) error }) (AdminUser, error) {
	var u AdminUser
	var avatar sql.NullString
	err := sc.Scan(&u.ID, &u.Email, &u.Name, &u.About, &avatar, &u.Platform, &u.DeviceName, &u.DeviceGeneration, &u.CreatedAt, &u.LastActiveAt, &u.DisabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if avatar.Valid {
		u.AvatarID = &avatar.String
	}
	return u, err
}

// AdminListUsers 分页列出用户。q 非空时按邮箱 / 昵称子串或 ID 前缀过滤；sort 为 "active" 时按最近活跃倒序，
// 否则按注册时间倒序；status 为 "disabled" / "active" 时只看已停用 / 正常账号。
func (s *Store) AdminListUsers(ctx context.Context, q, sort, status string, page, size int) ([]AdminUser, int, error) {
	where, args := "1=1", []any{}
	if q != "" {
		like := "%" + likeEscape(q) + "%"
		where += ` AND (email LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR id LIKE ? ESCAPE '\')`
		args = append(args, like, like, likeEscape(q)+"%")
	}
	switch status {
	case "disabled":
		where += " AND disabled_at > 0"
	case "active":
		where += " AND disabled_at = 0"
	}
	order := "created_at DESC, rowid DESC"
	if sort == "active" {
		order = "last_active_at DESC, rowid DESC"
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+adminUserCols+" FROM users WHERE "+where+" ORDER BY "+order+" LIMIT ? OFFSET ?",
		append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AdminUser{}
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// AdminUserGroup 是用户所在的一个群。
type AdminUserGroup struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Members int    `json:"members"`
}

// AdminUserDetail 是用户详情：资料加各类**数量与大小**（队列只有条数与字节数）。
type AdminUserDetail struct {
	AdminUser
	Friends         int              `json:"friends"`
	Groups          []AdminUserGroup `json:"groups"`
	Moments         int              `json:"moments"`
	QueueItems      int64            `json:"queue_items"`
	QueueBytes      int64            `json:"queue_bytes"`
	Attachments     int              `json:"attachments"`
	AttachmentBytes int64            `json:"attachment_bytes"`
	Sessions        int              `json:"sessions"` // 有效登录令牌数
	PendingReports  int              `json:"pending_reports"`
}

// AdminGetUser 查询用户详情。
func (s *Store) AdminGetUser(ctx context.Context, id string) (AdminUserDetail, error) {
	var d AdminUserDetail
	u, err := scanAdminUser(s.db.QueryRowContext(ctx, "SELECT "+adminUserCols+" FROM users WHERE id = ?", id))
	if err != nil {
		return d, err
	}
	d.AdminUser = u
	d.Groups = []AdminUserGroup{}
	counts := []struct {
		q    string
		dest []any
	}{
		{"SELECT COUNT(*) FROM friendships WHERE user_id = ?", []any{&d.Friends}},
		{"SELECT COUNT(*) FROM moments WHERE author_id = ?", []any{&d.Moments}},
		{"SELECT COALESCE(SUM(items), 0), COALESCE(SUM(bytes), 0) FROM queue_usage WHERE user_id = ?", []any{&d.QueueItems, &d.QueueBytes}},
		{"SELECT COUNT(*), COALESCE(SUM(size), 0) FROM attachments WHERE uploader_id = ?", []any{&d.Attachments, &d.AttachmentBytes}},
		{"SELECT COUNT(*) FROM tokens WHERE user_id = ?", []any{&d.Sessions}},
		{"SELECT COUNT(*) FROM reports WHERE target_type = 'user' AND status = 'pending' AND target_id = ?", []any{&d.PendingReports}},
	}
	for _, c := range counts {
		if err := s.db.QueryRowContext(ctx, c.q, id).Scan(c.dest...); err != nil {
			return d, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.name, gm.role, (SELECT COUNT(*) FROM group_members x WHERE x.group_id = g.id)
		FROM group_members gm JOIN groups g ON g.id = gm.group_id WHERE gm.user_id = ? ORDER BY gm.joined_at, gm.rowid`, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var g AdminUserGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Role, &g.Members); err != nil {
			return d, err
		}
		d.Groups = append(d.Groups, g)
	}
	return d, rows.Err()
}

// AccountState 在事务内按邮箱查询账号是否存在、是否已停用（登录流程在 Login 之前调用）。
func (t *Tx) AccountState(email string) (exists, disabled bool, err error) {
	var at int64
	err = t.tx.QueryRowContext(t.ctx, "SELECT disabled_at FROM users WHERE email = ?", email).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, at > 0, err
}

// SetUserDisabled 停用或启用账号。停用时同时作废全部登录令牌（在线连接由调用方在提交后踢下线）。
func (t *Tx) SetUserDisabled(id string, disabled bool) error {
	at := int64(0)
	if disabled {
		at = NowMillis()
	}
	res, err := t.tx.ExecContext(t.ctx, "UPDATE users SET disabled_at = ? WHERE id = ?", at, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if disabled {
		return t.RevokeTokens(id)
	}
	return nil
}

// RevokeTokens 作废用户的全部登录令牌（强制下线）。
func (t *Tx) RevokeTokens(id string) error {
	t.InvalidateUserTokens(id)
	_, err := t.tx.ExecContext(t.ctx, "DELETE FROM tokens WHERE user_id = ?", id)
	return err
}

// AdminGetUser 在事务内查询用户基本记录。
func (t *Tx) AdminGetUser(id string) (AdminUser, error) {
	return scanAdminUser(t.tx.QueryRowContext(t.ctx, "SELECT "+adminUserCols+" FROM users WHERE id = ?", id))
}

// DeletedUser 是删除账号的结果，供调用方在同一事务内发事件、提交后清理文件。
type DeletedUser struct {
	User     AdminUser
	Friends  []string // 原好友
	Groups   []string // 仍然存在、需要广播 group_updated 的群
	MediaIDs []string // 该用户的头像与朋友圈图片附件（提交后若不再被引用即可删除）
}

// AdminDeleteUser 在事务内删除账号并级联清理：好友关系、好友申请、拉黑、群成员（群主自动转让，空群解散）、
// 朋友圈（含点赞、评论、通知、可见范围名单）、离线队列、密钥、令牌、验证码。没有外键的表在这里显式清理；
// 别人评论里「回复 TA」的指向置空。
func (t *Tx) AdminDeleteUser(id string) (DeletedUser, error) {
	ctx, tx := t.ctx, t.tx
	var d DeletedUser
	t.InvalidateUserTokens(id)
	u, err := t.AdminGetUser(id)
	if err != nil {
		return d, err
	}
	d.User = u
	if d.Friends, err = queryIDs(ctx, tx, "SELECT friend_id FROM friendships WHERE user_id = ?", id); err != nil {
		return d, err
	}
	if d.MediaIDs, err = queryIDs(ctx, tx, `SELECT mm.attachment_id FROM moment_media mm JOIN moments m ON m.id = mm.moment_id WHERE m.author_id = ?
		UNION SELECT avatar_id FROM users WHERE id = ? AND avatar_id IS NOT NULL`, id, id); err != nil {
		return d, err
	}
	groups, err := queryIDs(ctx, tx, "SELECT group_id FROM group_members WHERE user_id = ?", id)
	if err != nil {
		return d, err
	}
	for _, gid := range groups {
		dissolved, err := t.RemoveMember(gid, id)
		if err != nil {
			return d, err
		}
		if dissolved {
			if _, err := tx.ExecContext(ctx, "DELETE FROM group_stats WHERE group_id = ?", gid); err != nil {
				return d, err
			}
			continue
		}
		d.Groups = append(d.Groups, gid)
	}
	stmts := []struct {
		q   string
		arg string
	}{
		{"DELETE FROM queue WHERE user_id = ?", id},
		{"DELETE FROM queue_usage WHERE user_id = ?", id},
		{"DELETE FROM moment_notifications WHERE user_id = ?", id},
		{"DELETE FROM moment_visibility WHERE user_id = ?", id},
		{"UPDATE moment_comments SET reply_to_id = NULL WHERE reply_to_id = ?", id}, // 别人回复他的评论保留，不再指向已删除的用户
		{"DELETE FROM user_activity WHERE user_id = ?", id},
		{"DELETE FROM auth_codes WHERE email = ?", u.Email},
		// 其余表（tokens、密钥、好友、申请、拉黑、群成员、动态及其点赞/评论/通知）经外键级联删除
		{"DELETE FROM users WHERE id = ?", id},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.arg); err != nil {
			return d, err
		}
	}
	return d, nil
}

// DeleteUnreferencedAttachments 删除 ids 中已不被任何头像（用户 / 群）或朋友圈动态引用的附件元数据，返回实际删除的 id
// （调用方再删除对应文件）。用于删除账号或删除违规动态之后立刻清掉明文图片，而不是等 90 天闲置清理。
func (s *Store) DeleteUnreferencedAttachments(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []string
	err := s.withTx(ctx, func(tx dbtx) error {
		var err error
		out, err = queryIDs(ctx, tx, "DELETE FROM attachments WHERE id IN ("+placeholders(len(ids))+`)
		AND NOT EXISTS (SELECT 1 FROM users u WHERE u.avatar_id = attachments.id)
		AND NOT EXISTS (SELECT 1 FROM groups g WHERE g.avatar_id = attachments.id)
		AND NOT EXISTS (SELECT 1 FROM moment_media mm WHERE mm.attachment_id = attachments.id) RETURNING id`, toArgs(ids)...)
		return err
	})
	return out, err
}

// PublicMedia 判断附件是否是服务端本来就以明文持有的图片：用户 / 群头像或朋友圈图片。
// 管理后台的缩略图接口只允许读取这类附件，聊天附件（端到端加密）一律不可读取。
func (s *Store) PublicMedia(ctx context.Context, id string) (bool, error) {
	return exists(ctx, s.db, `SELECT 1 WHERE EXISTS (SELECT 1 FROM moment_media WHERE attachment_id = ?)
		OR EXISTS (SELECT 1 FROM users WHERE avatar_id = ?) OR EXISTS (SELECT 1 FROM groups WHERE avatar_id = ?)`, id, id, id)
}

// AllUserIDs 在事务内返回全部未停用用户的 id（系统公告扇出）。
func (t *Tx) AllUserIDs() ([]string, error) {
	return queryIDs(t.ctx, t.tx, "SELECT id FROM users WHERE disabled_at = 0")
}
