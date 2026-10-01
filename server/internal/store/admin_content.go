package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------- 群组 ----------

// AdminGroup 是管理后台看到的群。Messages 是累计发送的群消息**条数**（不含内容）。
type AdminGroup struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	AvatarID      *string `json:"avatar_id"`
	Announcement  string  `json:"announcement"`
	OwnerID       string  `json:"owner_id"`
	OwnerName     string  `json:"owner_name"`
	Members       int     `json:"members"`
	CreatedAt     int64   `json:"created_at"`
	UpdatedAt     int64   `json:"updated_at"`
	Messages      int64   `json:"messages"`
	LastMessageAt int64   `json:"last_message_at"`
}

const adminGroupSelect = `SELECT g.id, g.name, g.avatar_id, g.announcement, g.owner_id, COALESCE(u.name, ''),
	(SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = g.id), g.created_at, g.updated_at,
	COALESCE(gs.messages, 0), COALESCE(gs.last_message_at, 0)
	FROM groups g LEFT JOIN users u ON u.id = g.owner_id LEFT JOIN group_stats gs ON gs.group_id = g.id`

func scanAdminGroup(sc interface{ Scan(...any) error }) (AdminGroup, error) {
	var g AdminGroup
	var avatar sql.NullString
	err := sc.Scan(&g.ID, &g.Name, &avatar, &g.Announcement, &g.OwnerID, &g.OwnerName, &g.Members, &g.CreatedAt, &g.UpdatedAt, &g.Messages, &g.LastMessageAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if avatar.Valid {
		g.AvatarID = &avatar.String
	}
	return g, err
}

// AdminListGroups 分页列出群（按创建时间倒序）；q 非空时按群名子串或 ID 前缀过滤。
func (s *Store) AdminListGroups(ctx context.Context, q string, page, size int) ([]AdminGroup, int, error) {
	where, args := "1=1", []any{}
	if q != "" {
		where = `(g.name LIKE ? ESCAPE '\' OR g.id LIKE ? ESCAPE '\')`
		args = append(args, "%"+likeEscape(q)+"%", likeEscape(q)+"%")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups g WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, adminGroupSelect+" WHERE "+where+" ORDER BY g.created_at DESC, g.rowid DESC LIMIT ? OFFSET ?",
		append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AdminGroup{}
	for rows.Next() {
		g, err := scanAdminGroup(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, g)
	}
	return out, total, rows.Err()
}

// AdminMember 是群成员及其资料。
type AdminMember struct {
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	JoinedAt int64  `json:"joined_at"`
}

// AdminGetGroup 查询群详情与成员列表。
func (s *Store) AdminGetGroup(ctx context.Context, id string) (AdminGroup, []AdminMember, error) {
	g, err := scanAdminGroup(s.db.QueryRowContext(ctx, adminGroupSelect+" WHERE g.id = ?", id))
	if err != nil {
		return g, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gm.user_id, u.name, u.email, gm.role, gm.joined_at FROM group_members gm
		JOIN users u ON u.id = gm.user_id WHERE gm.group_id = ? ORDER BY gm.joined_at, gm.rowid`, id)
	if err != nil {
		return g, nil, err
	}
	defer rows.Close()
	members := []AdminMember{}
	for rows.Next() {
		var m AdminMember
		if err := rows.Scan(&m.UserID, &m.Name, &m.Email, &m.Role, &m.JoinedAt); err != nil {
			return g, nil, err
		}
		members = append(members, m)
	}
	return g, members, rows.Err()
}

// DissolveGroup 在事务内解散群，返回解散前的成员 id（调用方向他们发 group_removed）。
func (t *Tx) DissolveGroup(gid string) ([]string, error) {
	members, err := queryIDs(t.ctx, t.tx, "SELECT user_id FROM group_members WHERE group_id = ?", gid)
	if err != nil {
		return nil, err
	}
	res, err := t.tx.ExecContext(t.ctx, "DELETE FROM groups WHERE id = ?", gid)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	_, err = t.tx.ExecContext(t.ctx, "DELETE FROM group_stats WHERE group_id = ?", gid)
	return members, err
}

// TransferOwner 在事务内把群主转让给 newOwner（必须已是成员，否则 ErrNotFound）；原群主降为管理员。
func (t *Tx) TransferOwner(gid, newOwner string) error {
	ctx, tx := t.ctx, t.tx
	if ok, err := exists(ctx, tx, "SELECT 1 FROM group_members WHERE group_id = ? AND user_id = ?", gid, newOwner); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, "UPDATE group_members SET role = 'admin' WHERE group_id = ? AND role = 'owner' AND user_id <> ?", gid, newOwner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE group_members SET role = 'owner' WHERE group_id = ? AND user_id = ?", gid, newOwner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE groups SET owner_id = ? WHERE id = ?", newOwner, gid); err != nil {
		return err
	}
	return touchGroup(ctx, tx, gid)
}

// ---------- 朋友圈审核 ----------

// AdminMoment 是管理后台看到的动态（朋友圈在服务端是明文）。
type AdminMoment struct {
	ID             string  `json:"id"`
	AuthorID       string  `json:"author_id"`
	AuthorName     string  `json:"author_name"`
	AuthorEmail    string  `json:"author_email"`
	Text           string  `json:"text"`
	Media          []Media `json:"media"`
	VisMode        string  `json:"vis_mode"`
	CreatedAt      int64   `json:"created_at"`
	Likes          int     `json:"likes"`
	Comments       int     `json:"comments"`
	PendingReports int     `json:"pending_reports"`
}

// AdminListMoments 分页列出动态。author 非空时只看该作者；reportedFirst 时有待处理举报的排在前面
// （举报数多的更靠前），其余按时间倒序；onlyReported 时只返回有待处理举报的动态。
func (s *Store) AdminListMoments(ctx context.Context, author string, reportedFirst, onlyReported bool, page, size int) ([]AdminMoment, int, error) {
	const pending = "(SELECT COUNT(*) FROM reports r WHERE r.target_type = 'moment' AND r.target_id = m.id AND r.status = 'pending')"
	where, args := "1=1", []any{}
	if author != "" {
		where += " AND m.author_id = ?"
		args = append(args, author)
	}
	if onlyReported {
		where += " AND EXISTS (SELECT 1 FROM reports r WHERE r.target_type = 'moment' AND r.target_id = m.id AND r.status = 'pending')"
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM moments m WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "m.created_at DESC, m.id DESC"
	if reportedFirst {
		order = "pending DESC, " + order
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.author_id, u.name, u.email, m.text, m.media, m.vis_mode, m.created_at,
		(SELECT COUNT(*) FROM moment_likes l WHERE l.moment_id = m.id),
		(SELECT COUNT(*) FROM moment_comments c WHERE c.moment_id = m.id), `+pending+` AS pending
		FROM moments m JOIN users u ON u.id = m.author_id WHERE `+where+" ORDER BY "+order+" LIMIT ? OFFSET ?",
		append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AdminMoment{}
	for rows.Next() {
		var m AdminMoment
		var media string
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.AuthorName, &m.AuthorEmail, &m.Text, &media, &m.VisMode, &m.CreatedAt, &m.Likes, &m.Comments, &m.PendingReports); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal([]byte(media), &m.Media); err != nil || m.Media == nil {
			m.Media = []Media{}
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// AdminComment 是管理后台看到的评论。
type AdminComment struct {
	ID             string `json:"id"`
	MomentID       string `json:"moment_id"`
	AuthorID       string `json:"author_id"`
	AuthorName     string `json:"author_name"`
	Text           string `json:"text"`
	CreatedAt      int64  `json:"created_at"`
	PendingReports int    `json:"pending_reports"`
}

// AdminMomentComments 返回一条动态的全部评论（按时间）。
func (s *Store) AdminMomentComments(ctx context.Context, momentID string) ([]AdminComment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.moment_id, c.author_id, u.name, c.text, c.created_at,
		(SELECT COUNT(*) FROM reports r WHERE r.target_type = 'comment' AND r.target_id = c.id AND r.status = 'pending')
		FROM moment_comments c JOIN users u ON u.id = c.author_id WHERE c.moment_id = ? ORDER BY c.created_at, c.rowid`, momentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminComment{}
	for rows.Next() {
		var c AdminComment
		if err := rows.Scan(&c.ID, &c.MomentID, &c.AuthorID, &c.AuthorName, &c.Text, &c.CreatedAt, &c.PendingReports); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AdminDeleteMoment 在事务内删除动态（级联点赞、评论、通知、图片关联），返回作者与它引用的图片附件 id。
func (t *Tx) AdminDeleteMoment(id string) (author string, mediaIDs []string, err error) {
	ctx, tx := t.ctx, t.tx
	if err = tx.QueryRowContext(ctx, "SELECT author_id FROM moments WHERE id = ?", id).Scan(&author); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return
	}
	if mediaIDs, err = queryIDs(ctx, tx, "SELECT attachment_id FROM moment_media WHERE moment_id = ?", id); err != nil {
		return
	}
	for _, q := range []string{"DELETE FROM moment_visibility WHERE moment_id = ?", "DELETE FROM moments WHERE id = ?"} {
		if _, err = tx.ExecContext(ctx, q, id); err != nil {
			return
		}
	}
	return
}

// AdminDeleteComment 在事务内删除评论及其通知，返回评论作者；不存在返回 ErrNotFound。
func (t *Tx) AdminDeleteComment(id string) (author string, err error) {
	ctx, tx := t.ctx, t.tx
	if err = tx.QueryRowContext(ctx, "SELECT author_id FROM moment_comments WHERE id = ?", id).Scan(&author); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return
	}
	for _, q := range []string{"DELETE FROM moment_notifications WHERE comment_id = ?", "DELETE FROM moment_comments WHERE id = ?"} {
		if _, err = tx.ExecContext(ctx, q, id); err != nil {
			return
		}
	}
	return
}

// ---------- 举报 ----------

// 举报对象类型。
const (
	ReportUser    = "user"
	ReportMoment  = "moment"
	ReportComment = "comment"
	ReportGroup   = "group"
)

// Report 是一条举报（管理后台 A10）。
type Report struct {
	ID           string `json:"id"`
	ReporterID   string `json:"reporter_id"`
	ReporterName string `json:"reporter_name,omitempty"`
	TargetType   string `json:"target_type"`
	TargetID     string `json:"target_id"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail"`
	Status       string `json:"status"`
	Action       string `json:"action"`
	Note         string `json:"note"`
	HandledBy    string `json:"handled_by"`
	HandledAt    int64  `json:"handled_at"`
	CreatedAt    int64  `json:"created_at"`
}

// CreateReport 写入举报。同一举报人对同一对象已有待处理举报时不重复写入，返回已有那条的 id 与 created=false。
func (s *Store) CreateReport(ctx context.Context, r Report) (id string, created bool, err error) {
	err = s.withTx(ctx, func(tx dbtx) error {
		e := tx.QueryRowContext(ctx, "SELECT id FROM reports WHERE reporter_id = ? AND target_type = ? AND target_id = ? AND status = 'pending'",
			r.ReporterID, r.TargetType, r.TargetID).Scan(&id)
		if e == nil {
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		id, created = NewID(), true
		_, e = tx.ExecContext(ctx, "INSERT INTO reports (id, reporter_id, target_type, target_id, reason, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			id, r.ReporterID, r.TargetType, r.TargetID, r.Reason, r.Detail, NowMillis())
		return e
	})
	return
}

const reportCols = "r.id, r.reporter_id, COALESCE(u.name, ''), r.target_type, r.target_id, r.reason, r.detail, r.status, r.action, r.note, r.handled_by, r.handled_at, r.created_at"

func scanReport(sc interface{ Scan(...any) error }) (Report, error) {
	var r Report
	err := sc.Scan(&r.ID, &r.ReporterID, &r.ReporterName, &r.TargetType, &r.TargetID, &r.Reason, &r.Detail, &r.Status, &r.Action, &r.Note, &r.HandledBy, &r.HandledAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// AdminListReports 分页列出举报；status 为 "pending" / "resolved"，空表示全部。待处理的按时间正序（先到先处理），其余倒序。
func (s *Store) AdminListReports(ctx context.Context, status string, page, size int) ([]Report, int, error) {
	where, args := "1=1", []any{}
	order := "r.created_at DESC"
	if status != "" {
		where = "r.status = ?"
		args = append(args, status)
		if status == "pending" {
			order = "r.created_at ASC"
		}
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM reports r WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+reportCols+" FROM reports r LEFT JOIN users u ON u.id = r.reporter_id WHERE "+where+
		" ORDER BY "+order+", r.rowid LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetReport 查询一条举报。
func (s *Store) GetReport(ctx context.Context, id string) (Report, error) {
	return scanReport(s.db.QueryRowContext(ctx, "SELECT "+reportCols+" FROM reports r LEFT JOIN users u ON u.id = r.reporter_id WHERE r.id = ?", id))
}

// ReportTarget 是被举报对象的摘要（供后台展示；对象已被删除时 Exists=false）。
type ReportTarget struct {
	Exists   bool   `json:"exists"`
	Summary  string `json:"summary"`   // 昵称 / 动态文字 / 评论文字 / 群名
	OwnerID  string `json:"owner_id"`  // 责任账号：用户本人 / 动态或评论作者 / 群主
	Owner    string `json:"owner"`     // 责任账号昵称
	MomentID string `json:"moment_id"` // 评论所在的动态
}

// ReportTargetInfo 解析被举报对象的摘要与责任账号。
func (s *Store) ReportTargetInfo(ctx context.Context, typ, id string) (ReportTarget, error) {
	var t ReportTarget
	var q string
	switch typ {
	case ReportUser:
		q = "SELECT u.name, u.id, u.name, '' FROM users u WHERE u.id = ?"
	case ReportMoment:
		q = "SELECT m.text, u.id, u.name, m.id FROM moments m JOIN users u ON u.id = m.author_id WHERE m.id = ?"
	case ReportComment:
		q = "SELECT c.text, u.id, u.name, c.moment_id FROM moment_comments c JOIN users u ON u.id = c.author_id WHERE c.id = ?"
	case ReportGroup:
		q = "SELECT g.name, g.owner_id, COALESCE(u.name, ''), '' FROM groups g LEFT JOIN users u ON u.id = g.owner_id WHERE g.id = ?"
	default:
		return t, nil
	}
	err := s.db.QueryRowContext(ctx, q, id).Scan(&t.Summary, &t.OwnerID, &t.Owner, &t.MomentID)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil
	}
	t.Exists = err == nil
	return t, err
}

// ReportTargets 批量解析一页举报的对象摘要：每种对象类型一条查询（而不是每行举报一条）。
// 返回值以 "类型\x00id" 为键；已被删除的对象不在结果里（调用方按 Exists=false 处理）。
func (s *Store) ReportTargets(ctx context.Context, reports []Report) (map[string]ReportTarget, error) {
	byType := map[string][]string{}
	seen := map[string]bool{}
	for _, r := range reports {
		k := r.TargetType + "\x00" + r.TargetID
		if !seen[k] {
			seen[k] = true
			byType[r.TargetType] = append(byType[r.TargetType], r.TargetID)
		}
	}
	queries := map[string]string{
		ReportUser:    "SELECT u.id, u.name, u.id, u.name, '' FROM users u WHERE u.id IN (%s)",
		ReportMoment:  "SELECT m.id, m.text, u.id, u.name, m.id FROM moments m JOIN users u ON u.id = m.author_id WHERE m.id IN (%s)",
		ReportComment: "SELECT c.id, c.text, u.id, u.name, c.moment_id FROM moment_comments c JOIN users u ON u.id = c.author_id WHERE c.id IN (%s)",
		ReportGroup:   "SELECT g.id, g.name, g.owner_id, COALESCE(u.name, ''), '' FROM groups g LEFT JOIN users u ON u.id = g.owner_id WHERE g.id IN (%s)",
	}
	out := make(map[string]ReportTarget, len(seen))
	for typ, ids := range byType {
		q, ok := queries[typ]
		if !ok {
			continue
		}
		rows, err := s.rd.QueryContext(ctx, fmt.Sprintf(q, placeholders(len(ids))), toArgs(ids)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			t := ReportTarget{Exists: true}
			if err := rows.Scan(&id, &t.Summary, &t.OwnerID, &t.Owner, &t.MomentID); err != nil {
				rows.Close()
				return nil, err
			}
			out[typ+"\x00"+id] = t
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ResolveReports 在事务内把针对同一对象的全部待处理举报标记为已处理，返回处理条数。
func (t *Tx) ResolveReports(targetType, targetID, action, note, by string) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx, `UPDATE reports SET status = 'resolved', action = ?, note = ?, handled_by = ?, handled_at = ?
		WHERE target_type = ? AND target_id = ? AND status = 'pending'`, action, note, by, NowMillis(), targetType, targetID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountPendingReports 返回待处理举报数。
func (s *Store) CountPendingReports(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM reports WHERE status = 'pending'").Scan(&n)
	return n, err
}

// ---------- 邀请码 ----------

// ErrInviteInvalid 邀请码不存在、已作废、已过期或次数用完。
var ErrInviteInvalid = errors.New("invite invalid")

// Invite 是一个邀请码（管理后台 A11）。
type Invite struct {
	Code      string `json:"code"`
	MaxUses   int    `json:"max_uses"`
	Used      int    `json:"used"`
	ExpiresAt int64  `json:"expires_at"` // 0 = 不过期
	Revoked   bool   `json:"revoked"`
	Note      string `json:"note"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
}

// NormalizeInvite 规范化邀请码：去空白、转大写。
func NormalizeInvite(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// CreateInvites 批量写入邀请码，并在同一事务内追加审计记录。
func (s *Store) CreateInvites(ctx context.Context, invites []Invite, audit AuditEntry) error {
	now := NowMillis()
	return s.withTx(ctx, func(tx dbtx) error {
		for _, in := range invites {
			if _, err := tx.ExecContext(ctx, "INSERT INTO invite_codes (code, max_uses, expires_at, note, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)",
				in.Code, in.MaxUses, in.ExpiresAt, in.Note, in.CreatedBy, now); err != nil {
				return err
			}
		}
		return appendAudit(ctx, tx, audit)
	})
}

// ListInvites 分页列出邀请码（新的在前）。
func (s *Store) ListInvites(ctx context.Context, page, size int) ([]Invite, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM invite_codes").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT code, max_uses, used, expires_at, revoked, note, created_by, created_at FROM invite_codes
		ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var in Invite
		if err := rows.Scan(&in.Code, &in.MaxUses, &in.Used, &in.ExpiresAt, &in.Revoked, &in.Note, &in.CreatedBy, &in.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, in)
	}
	return out, total, rows.Err()
}

// RevokeInvite 作废邀请码；不存在返回 ErrNotFound。
func (s *Store) RevokeInvite(ctx context.Context, code string, audit AuditEntry) error {
	return s.withTx(ctx, func(tx dbtx) error {
		res, err := tx.ExecContext(ctx, "UPDATE invite_codes SET revoked = 1 WHERE code = ?", code)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return appendAudit(ctx, tx, audit)
	})
}

// InviteUse 是邀请码的一次使用记录。
type InviteUse struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	UsedAt int64  `json:"used_at"`
}

// InviteUses 返回邀请码的使用记录（按时间）。
func (s *Store) InviteUses(ctx context.Context, code string) ([]InviteUse, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT user_id, email, used_at FROM invite_uses WHERE code = ? ORDER BY used_at, rowid", code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InviteUse{}
	for rows.Next() {
		var u InviteUse
		if err := rows.Scan(&u.UserID, &u.Email, &u.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ConsumeInvite 在注册事务内占用邀请码的一次使用名额（条件更新，并发注册不会超发）；
// 邀请码不可用返回 ErrInviteInvalid。
func (t *Tx) ConsumeInvite(code string) error {
	res, err := t.tx.ExecContext(t.ctx, `UPDATE invite_codes SET used = used + 1
		WHERE code = ? AND revoked = 0 AND used < max_uses AND (expires_at = 0 OR expires_at > ?)`, code, NowMillis())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteInvalid
	}
	return nil
}

// RecordInviteUse 在注册事务内记录邀请码被谁使用。
func (t *Tx) RecordInviteUse(code, userID, email string) error {
	_, err := t.tx.ExecContext(t.ctx, "INSERT INTO invite_uses (code, user_id, email, used_at) VALUES (?, ?, ?, ?)", code, userID, email, NowMillis())
	return err
}

// ---------- 系统公告 ----------

// Announcement 是一条系统公告（管理后台 A12）。
type Announcement struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // 0 = 不过期
	CreatedBy string `json:"created_by,omitempty"` // 仅后台可见
}

// CreateAnnouncement 在事务内写入公告（扇出尚未开始：fanout_done=0，由 FanoutAnnouncement 分批完成）。
func (t *Tx) CreateAnnouncement(a Announcement) error {
	_, err := t.tx.ExecContext(t.ctx, "INSERT INTO announcements (id, title, body, created_at, expires_at, created_by, fanout_done) VALUES (?, ?, ?, ?, ?, ?, 0)",
		a.ID, a.Title, a.Body, a.CreatedAt, a.ExpiresAt, a.CreatedBy)
	return err
}

// AnnouncementEvent 生成公告事件的负载：{"id","type":"announcement","title","body","created_at"[,"expires_at"]}。事件 id 就是公告 id。
func AnnouncementEvent(a Announcement) ([]byte, error) {
	ev := map[string]any{"id": a.ID, "type": "announcement", "title": a.Title, "body": a.Body, "created_at": a.CreatedAt}
	if a.ExpiresAt > 0 {
		ev["expires_at"] = a.ExpiresAt
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// AnnouncementFanoutBatch 是公告事件每个事务扇出的用户数。
const AnnouncementFanoutBatch = 500

// FanoutAnnouncement 把公告事件分批写进全部未停用用户的队列，返回累计的接收人数。
//
// 每批一个小事务（按用户 id 顺序推进，进度记在公告行上），批与批之间其他写入可以插进来：几十万用户的公告也不会让全库停顿。
// 进程中途退出时进度已落库，启动时由 ResumeAnnouncementFanouts 续发；事件 id 就是公告 id，重复写入被忽略，所以续发是幂等的。
// 公告在扇出途中被撤下则停止。
func (s *Store) FanoutAnnouncement(ctx context.Context, id string) (int, error) {
	total := 0
	for {
		done := false
		err := s.Update(ctx, func(t *Tx) error {
			var a Announcement
			var cursor string
			var finished int
			err := t.tx.QueryRowContext(ctx, "SELECT id, title, body, created_at, expires_at, fanout_done, fanout_cursor, recipients FROM announcements WHERE id = ?", id).
				Scan(&a.ID, &a.Title, &a.Body, &a.CreatedAt, &a.ExpiresAt, &finished, &cursor, &total)
			if isNoRows(err) || (err == nil && finished == 1) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			payload, err := AnnouncementEvent(a)
			if err != nil {
				return err
			}
			ids, err := queryIDs(ctx, t.tx, "SELECT id FROM users WHERE disabled_at = 0 AND id > ? ORDER BY id LIMIT ?", cursor, AnnouncementFanoutBatch)
			if err != nil {
				return err
			}
			items := make([]QueueInsert, len(ids))
			for i, uid := range ids {
				items[i] = QueueInsert{UserID: uid, ID: a.ID, Kind: KindEvent, Payload: payload, Shared: true}
			}
			if _, err := t.Enqueue(items, QueueLimits{}); err != nil {
				return err
			}
			total += len(ids)
			if len(ids) < AnnouncementFanoutBatch {
				done = true
				_, err = t.tx.ExecContext(ctx, "UPDATE announcements SET fanout_done = 1, recipients = ? WHERE id = ?", total, id)
				return err
			}
			_, err = t.tx.ExecContext(ctx, "UPDATE announcements SET fanout_cursor = ?, recipients = ? WHERE id = ?", ids[len(ids)-1], total, id)
			return err
		})
		if err != nil || done {
			return total, err
		}
	}
}

// ResumeAnnouncementFanouts 续发上次进程退出时没扇出完的公告（启动时调用），返回续发的公告数。
func (s *Store) ResumeAnnouncementFanouts(ctx context.Context) (int, error) {
	ids, err := s.queryIDs(ctx, "SELECT id FROM announcements WHERE fanout_done = 0 ORDER BY created_at")
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.FanoutAnnouncement(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// ListAnnouncements 按时间倒序返回 created_at < before 的公告，最多 limit 条。
// activeOnly 时跳过已过截止时间的，并且不返回 created_by（客户端接口用）。
func (s *Store) ListAnnouncements(ctx context.Context, before int64, limit int, activeOnly bool) ([]Announcement, error) {
	where, args := "created_at < ?", []any{before}
	if activeOnly {
		where += " AND (expires_at = 0 OR expires_at > ?)"
		args = append(args, NowMillis())
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, title, body, created_at, expires_at, created_by FROM announcements WHERE "+where+
		" ORDER BY created_at DESC, rowid DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Announcement{}
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.CreatedAt, &a.ExpiresAt, &a.CreatedBy); err != nil {
			return nil, err
		}
		if activeOnly {
			a.CreatedBy = ""
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAnnouncement 撤下公告（不再出现在 GET /v1/announcements；已推送的事件不回收）。
func (s *Store) DeleteAnnouncement(ctx context.Context, id string, audit AuditEntry) error {
	return s.withTx(ctx, func(tx dbtx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM announcements WHERE id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return appendAudit(ctx, tx, audit)
	})
}
